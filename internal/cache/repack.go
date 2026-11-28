package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/logging"
)

type RepackOptions struct {
	PackThreshold int
	Timeout       time.Duration
	// GCTimeout bounds a cruft-repack collection, which rewrites every
	// pack; zero makes it share Timeout.
	GCTimeout   time.Duration
	GCInterval  time.Duration
	GCRetention time.Duration
}

const MinimumGCRetention = 14 * 24 * time.Hour

func packCount(dir string) int {
	files, _ := filepath.Glob(filepath.Join(dir, "objects", "pack", "*.pack"))
	return len(files)
}

func regularPackCount(dir string) int {
	files, _ := filepath.Glob(filepath.Join(dir, "objects", "pack", "*.pack"))
	count := 0
	for _, file := range files {
		if _, err := os.Stat(strings.TrimSuffix(file, ".pack") + ".mtimes"); os.IsNotExist(err) {
			count++
		}
	}
	return count
}

// Caller holds mu. Idle-only maintenance shares the same gate as hydration,
// ref reconciliation and serving views; queued requests also pin repositories.
func (s *Store) queueRepackLocked(e *entry) {
	if s.repackQueue == nil || s.maintenance.Err() != nil || e.repackPending || e.users != 0 {
		return
	}
	// The pack count comes from the writer's last rescan: reading the
	// directory here put filesystem work on the store mutex of every
	// request whose lease made a scope idle.
	if !s.gcDueLocked(e, time.Now()) && !(e.regularPacks >= s.repack.PackThreshold && e.repack.Due(time.Now())) {
		return
	}
	select {
	case s.repackQueue <- e.key:
		e.repackPending = true
	default:
	}
}

// gcDueLocked reports whether the collection interval has elapsed since the
// last successful collection and the previous attempt is not backing off.
func (s *Store) gcDueLocked(e *entry, now time.Time) bool {
	return now.Sub(e.lastGC) >= s.repack.GCInterval && e.gc.Due(now)
}

func (s *Store) maintain() {
	defer s.workers.Done()
	ticker := time.NewTicker(min(s.repack.GCInterval, time.Minute))
	defer ticker.Stop()
	for {
		if s.maintenance.Err() != nil {
			return
		}
		select {
		case <-s.maintenance.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			for _, e := range s.entries {
				if e.size > 0 {
					s.queueRepackLocked(e)
				}
			}
			s.mu.Unlock()
		case key := <-s.repackQueue:
			s.mu.Lock()
			e := s.entries[key]
			if e == nil {
				s.mu.Unlock()
				continue
			}
			e.repackPending = false
			if e.users != 0 || e.writer || e.readers != 0 {
				s.mu.Unlock()
				continue
			}
			now := time.Now()
			collect := s.gcDueLocked(e, now)
			if !collect && !e.repack.Due(now) {
				s.mu.Unlock()
				continue
			}
			e.users++
			e.writer = true
			timeout := s.repack.Timeout
			if collect {
				timeout = s.repack.GCTimeout
			}
			ctx, cancel := context.WithTimeout(s.maintenance, timeout)
			// Let queued requests preempt long maintenance; it retries later.
			e.cancelMaint = cancel
			s.mu.Unlock()
			l := &Lease{Dir: filepath.Join(s.Root, "repos", key, "repo.git"), store: s, entry: e}
			operation := "repack"
			if collect {
				operation = "gc"
			}
			ctx = logging.WithLogger(ctx, s.Git.Logger().With("cache_key", key, "operation", operation))
			started := time.Now()
			var changed bool
			var err error
			if collect {
				changed, err = l.Collect(ctx)
			} else if regularPackCount(l.Dir) < s.repack.PackThreshold {
				// The cached count ran ahead of the directory; nothing to do.
				cancel()
				s.mu.Lock()
				e.cancelMaint = nil
				s.mu.Unlock()
				l.release(false)
				continue
			} else {
				changed, err = l.Repack(ctx)
			}
			// A queued request cancels maintenance to proceed. That is
			// neither a failure nor a success: the attempt counts for
			// nothing and the next trigger retries at the same delay. The
			// cause is read before the deadline is released.
			preempted := errors.Is(ctx.Err(), context.Canceled) && s.maintenance.Err() == nil
			cancel()
			attrs := []slog.Attr{slog.Int64("duration_ms", time.Since(started).Milliseconds()), slog.Bool("changed", changed)}
			if err != nil {
				logging.Failure(ctx, s.Git.Logger(), "cache maintenance failed", err, attrs...)
			} else {
				level := slog.LevelInfo
				if !changed {
					level = slog.LevelDebug
				}
				logging.Logger(ctx, s.Git.Logger()).LogAttrs(ctx, level, "cache maintenance completed", attrs...)
			}
			s.mu.Lock()
			e.cancelMaint = nil
			if !preempted {
				backoff := &e.repack
				if collect {
					backoff = &e.gc
					switch {
					case err != nil:
						s.gcFailed++
					case changed:
						s.gcs++
					default:
						s.gcSkipped++
					}
				} else {
					switch {
					case err != nil:
						s.repackFailed++
					case changed:
						s.repacks++
					default:
						s.repackSkipped++
					}
				}
				// A capacity shortage is retried on the same schedule as a
				// failure: deciding it again costs a full object walk, which
				// the minute-based scan would otherwise repeat forever.
				if err != nil || !changed {
					backoff.Fail(time.Now(), maintenanceFailureBackoff, failureBackoffLimit)
				} else {
					backoff.Reset()
				}
			}
			s.mu.Unlock()
			// Release must not immediately requeue failed/no-op geometric packs.
			l.release(false)
		}
	}
}

// Repack builds a private pack set with hardlinks to immutable object files.
// Git may delete those links, while the published repository remains intact.
// New bytes consume a reservation; growth is checked every 200ms and at exit.
func (l *Lease) Repack(ctx context.Context) (bool, error) {
	if regularPackCount(l.Dir) < l.store.repack.PackThreshold {
		return false, nil
	}
	return l.rewrite(ctx, false, time.Now())
}

func (l *Lease) Collect(ctx context.Context) (bool, error) {
	return l.rewrite(ctx, true, time.Now())
}

func (l *Lease) rewrite(ctx context.Context, collect bool, now time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !l.complete(ctx, nil, true) {
		return false, fmt.Errorf("cannot repack an incomplete repository")
	}
	oldFiles, err := os.ReadDir(filepath.Join(l.Dir, "objects", "pack"))
	if err != nil {
		return false, err
	}
	objectBytes, err := directorySize(filepath.Join(l.Dir, "objects"))
	if err != nil {
		return false, err
	}
	budget := min(objectBytes+(1<<20), l.store.maxBytes-l.entry.size)
	if budget <= 0 {
		return false, nil
	}
	r, err := l.reserve(budget)
	if err != nil {
		return false, nil
	}
	defer r.release()
	stage, err := os.MkdirTemp(l.store.Tmp, "repack-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(stage)
	// A collection rebuilds the commit-graph in staging; graphReady marks
	// the one that passed admission and is to be installed.
	var previousGraph int64
	if info, err := os.Stat(filepath.Join(l.Dir, "objects", "info", "commit-graph")); err == nil {
		previousGraph = info.Size()
	}
	graph, graphReady := filepath.Join(stage, "objects", "info", "commit-graph"), false
	shared := make(map[string]os.FileInfo)
	if err := filepath.WalkDir(l.Dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		relative, err := filepath.Rel(l.Dir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(stage, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("repository maintenance requires regular files")
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		// Commit-graph files are rebuilt by collection, never copied: one can
		// be large, and duplicating it would read it into memory.
		if len(parts) >= 3 && parts[0] == "objects" && parts[1] == "info" && (parts[2] == "commit-graph" || parts[2] == "commit-graphs") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		immutable := len(parts) == 3 && parts[0] == "objects" &&
			(len(parts[1]) == 2 || (parts[1] == "pack" &&
				(strings.HasSuffix(parts[2], ".pack") || strings.HasSuffix(parts[2], ".idx") || strings.HasSuffix(parts[2], ".rev") || strings.HasSuffix(parts[2], ".bitmap"))))
		if immutable {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if err := os.Link(path, target); err != nil {
				return err
			}
			shared[relative] = info
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0600)
	}); err != nil {
		return false, err
	}
	newBytes := func() (int64, error) {
		var size int64
		err := filepath.WalkDir(stage, func(path string, entry os.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			info, err := entry.Info()
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(stage, path)
			if err != nil {
				return err
			}
			if old := shared[relative]; old == nil || !os.SameFile(old, info) {
				size += info.Size()
			}
			return nil
		})
		return size, err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				size, err := newBytes()
				if err != nil {
					cancel(err)
					return
				}
				if size > budget {
					cancel(ErrCapacity)
					return
				}
			}
		}
	}()
	var expired []string
	if collect {
		expired, err = l.expireRetained(ctx, stage, now)
		if err == nil {
			err = l.store.Git.Run(ctx, stage, nil, io.Discard, nil, "repack", "-a", "-d", "--cruft", "--cruft-expiration="+now.Add(-l.store.repack.GCRetention).UTC().Format(time.RFC3339))
		}
		if err == nil {
			err = l.store.Git.Run(ctx, stage, nil, io.Discard, nil, "prune", "--expire="+now.Add(-l.store.repack.GCRetention).UTC().Format(time.RFC3339))
		}
	} else {
		args := []string{"repack", "--geometric=2", "-d"}
		for _, file := range oldFiles {
			if strings.HasSuffix(file.Name(), ".mtimes") {
				args = append(args, "--keep-pack="+strings.TrimSuffix(file.Name(), ".mtimes")+".pack")
			}
		}
		err = l.store.Git.Run(ctx, stage, nil, io.Discard, nil, args...)
	}
	close(done)
	<-stopped
	if cause := context.Cause(ctx); cause != nil {
		return false, cause
	}
	if err != nil {
		return false, err
	}
	// The objects are judged against their reservation first; the graph is
	// written afterwards, so its bytes never fail the repack they follow.
	size, err := newBytes()
	if err != nil {
		return false, err
	}
	if size > budget {
		return false, ErrCapacity
	}
	if collect {
		// Collection is the one pass that already touches every commit, so
		// it is where the commit-graph is rebuilt. Ancestry checks (client
		// haves, include-tag targets, the pinned-root index) then stop at
		// generation boundaries instead of walking whole histories.
		if err := l.store.Git.Run(ctx, stage, nil, io.Discard, nil, "commit-graph", "write", "--reachable"); err == nil {
			// The graph is optional capacity: it is admitted only when its
			// growth over the graph it replaces fits, and otherwise left
			// out rather than failing the collection it accompanies.
			if info, err := os.Stat(graph); err == nil {
				if r, err := l.reserve(max(info.Size()-previousGraph, 0)); err == nil {
					defer r.release()
					graphReady = true
				} else {
					_ = os.Remove(graph)
				}
			}
		} else if ctx.Err() != nil {
			return false, err
		} else {
			// The graph only shortens walks; the repository is correct
			// without it and the next collection tries again.
			logging.Failure(ctx, l.store.Git.Logger(), "commit-graph generation skipped", err, slog.String("cache_key", l.entry.key))
		}
	}
	if err := l.store.Git.Run(ctx, stage, nil, io.Discard, nil, "rev-list", "--objects", "--missing=error", "--quiet", "--all"); err != nil {
		return false, err
	}
	files, err := os.ReadDir(filepath.Join(stage, "objects", "pack"))
	if err != nil {
		return false, err
	}
	keep := make(map[string]bool)
	changed := false
	// Install pack data before indexes and auxiliary metadata.
	for _, suffix := range []string{".pack", ".idx", ".rev", ".bitmap", ".mtimes"} {
		for _, file := range files {
			if !strings.HasSuffix(file.Name(), suffix) {
				continue
			}
			keep[file.Name()] = true
			source := filepath.Join(stage, "objects", "pack", file.Name())
			target := filepath.Join(l.Dir, "objects", "pack", file.Name())
			info, err := file.Info()
			if err != nil {
				return false, err
			}
			if old := shared[filepath.Join("objects", "pack", file.Name())]; old != nil && os.SameFile(old, info) {
				continue
			}
			if err := os.Rename(source, target); err != nil {
				return false, err
			}
			changed = true
		}
	}
	if graphReady {
		// Install the rebuilt graph like pack data, by rename, so readers
		// never observe a partially written file. Git publishes the graph
		// read-only, and Windows refuses to rename over such a file, so the
		// previous one is unlinked first; the exclusive gate keeps readers
		// away from the gap.
		target := filepath.Join(l.Dir, "objects", "info", "commit-graph")
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return false, err
		}
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return false, err
		}
		if err := os.Rename(graph, target); err != nil {
			return false, err
		}
		changed = true
	}
	if collect {
		// Remove expired grace pins only after all retained pack data is installed.
		if len(expired) > 0 {
			var updates strings.Builder
			pins := make([]string, 0, len(expired))
			for _, oid := range expired {
				fmt.Fprintf(&updates, "delete refs/cache/retained/%s %s\n", oid, oid)
				pins = append(pins, retainedPrefix+oid)
			}
			if err := clearStaleRefWrites(l.Dir, pins); err != nil {
				return false, err
			}
			if err := l.store.Git.Run(ctx, l.Dir, strings.NewReader(updates.String()), io.Discard, nil, "update-ref", "--stdin"); err != nil {
				return false, err
			}
		}
		data, err := os.ReadFile(filepath.Join(stage, retentionFile))
		if err != nil {
			return false, err
		}
		if err := atomicFile(l.Dir, retentionFile, data); err != nil {
			return false, err
		}
	}
	// MIDX cannot reference packs removed below. Drop it and let Git use per-pack
	// indexes; no MIDX is generated by the configured command.
	if err := os.RemoveAll(filepath.Join(l.Dir, "objects", "pack", "multi-pack-index")); err != nil {
		return false, err
	}
	if err := os.RemoveAll(filepath.Join(l.Dir, "objects", "pack", "multi-pack-index.d")); err != nil {
		return false, err
	}
	for _, file := range oldFiles {
		if !(strings.HasSuffix(file.Name(), ".pack") || strings.HasSuffix(file.Name(), ".idx") || strings.HasSuffix(file.Name(), ".rev") || strings.HasSuffix(file.Name(), ".bitmap") || strings.HasSuffix(file.Name(), ".mtimes")) {
			continue
		}
		if !keep[file.Name()] {
			if err := os.Remove(filepath.Join(l.Dir, "objects", "pack", file.Name())); err != nil && !os.IsNotExist(err) {
				return false, err
			}
			changed = true
		}
	}
	if collect {
		for relative := range shared {
			parts := strings.Split(filepath.ToSlash(relative), "/")
			if len(parts) != 3 || parts[0] != "objects" || len(parts[1]) != 2 {
				continue
			}
			if _, err := os.Stat(filepath.Join(stage, relative)); os.IsNotExist(err) {
				if err := os.Remove(filepath.Join(l.Dir, relative)); err != nil && !os.IsNotExist(err) {
					return false, err
				}
				changed = true
			} else if err != nil {
				return false, err
			}
		}
		if err := atomicFile(l.Dir, gcFile, []byte(fmt.Sprintf("%d\n", now.Unix()))); err != nil {
			return false, err
		}
		l.entry.lastGC = now
		changed = true
	}
	return changed, nil
}
