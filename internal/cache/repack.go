package cache

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type RepackOptions struct {
	PackThreshold int
	Timeout       time.Duration
}

func packCount(dir string) int {
	files, _ := filepath.Glob(filepath.Join(dir, "objects", "pack", "*.pack"))
	return len(files)
}

// Caller holds mu. Idle-only maintenance shares the same gate as hydration,
// ref reconciliation and serving views; queued requests also pin repositories.
func (s *Store) queueRepackLocked(e *entry) {
	if s.repackQueue == nil || s.maintenance.Err() != nil || e.repackPending || e.users != 0 {
		return
	}
	if packCount(filepath.Join(s.Root, "repos", e.key, "repo.git")) < s.repack.PackThreshold {
		return
	}
	select {
	case s.repackQueue <- e.key:
		e.repackPending = true
	default:
	}
}

func (s *Store) maintain() {
	defer s.workers.Done()
	for {
		select {
		case <-s.maintenance.Done():
			return
		case key := <-s.repackQueue:
			s.mu.Lock()
			e := s.entries[key]
			if e == nil {
				s.mu.Unlock()
				continue
			}
			e.repackPending = false
			if e.users != 0 {
				s.mu.Unlock()
				continue
			}
			e.users++
			e.gate <- struct{}{}
			s.mu.Unlock()
			l := &Lease{Dir: filepath.Join(s.Root, "repos", key, "repo.git"), store: s, entry: e}
			ctx, cancel := context.WithTimeout(s.maintenance, s.repack.Timeout)
			changed, err := l.Repack(ctx)
			cancel()
			s.mu.Lock()
			switch {
			case err != nil:
				s.repackFailed++
			case changed:
				s.repacks++
			default:
				s.repackSkipped++
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
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if packCount(l.Dir) < l.store.repack.PackThreshold {
		return false, nil
	}
	if !l.complete(ctx, nil, true) {
		return false, fmt.Errorf("cannot repack an incomplete repository")
	}
	oldFiles, err := os.ReadDir(filepath.Join(l.Dir, "objects", "pack"))
	if err != nil {
		return false, err
	}
	var packBytes int64
	for _, file := range oldFiles {
		if file.Type().IsRegular() {
			info, err := file.Info()
			if err != nil {
				return false, err
			}
			packBytes += info.Size()
		}
	}
	budget := min(packBytes+(1<<20), l.store.maxBytes-l.entry.size)
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
	err = l.store.Git.Run(ctx, stage, nil, io.Discard, nil, "repack", "--geometric=2", "-d")
	close(done)
	<-stopped
	if cause := context.Cause(ctx); cause != nil {
		return false, cause
	}
	if err != nil {
		return false, err
	}
	size, err := newBytes()
	if err != nil {
		return false, err
	}
	if size > budget {
		return false, ErrCapacity
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
	for _, suffix := range []string{".pack", ".idx", ".rev", ".bitmap"} {
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
	// MIDX cannot reference packs removed below. Drop it and let Git use per-pack
	// indexes; no MIDX is generated by the configured command.
	if err := os.RemoveAll(filepath.Join(l.Dir, "objects", "pack", "multi-pack-index")); err != nil {
		return false, err
	}
	if err := os.RemoveAll(filepath.Join(l.Dir, "objects", "pack", "multi-pack-index.d")); err != nil {
		return false, err
	}
	for _, file := range oldFiles {
		if !(strings.HasSuffix(file.Name(), ".pack") || strings.HasSuffix(file.Name(), ".idx") || strings.HasSuffix(file.Name(), ".rev") || strings.HasSuffix(file.Name(), ".bitmap")) {
			continue
		}
		if !keep[file.Name()] {
			if err := os.Remove(filepath.Join(l.Dir, "objects", "pack", file.Name())); err != nil && !os.IsNotExist(err) {
				return false, err
			}
			changed = true
		}
	}
	return changed, nil
}
