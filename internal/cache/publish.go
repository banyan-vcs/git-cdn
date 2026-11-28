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

func (l *Lease) fetchBounded(ctx context.Context, stage string, env map[string]string, args ...string) error {
	return l.runBounded(ctx, stage, func(ctx context.Context) error { return l.store.Git.Run(ctx, stage, nil, io.Discard, env, args...) })
}

func (l *Lease) runBounded(ctx context.Context, stage string, run func(context.Context) error) error {
	size, err := directorySize(stage)
	if err != nil {
		return err
	}
	if size > l.store.maxBytes {
		return ErrCapacity
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	done := make(chan struct{})
	stopped := make(chan struct{})
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
				size, err := directorySize(stage)
				if err != nil {
					cancel(err)
					return
				}
				if size > l.store.maxBytes {
					cancel(ErrCapacity)
					return
				}
			}
		}
	}()
	err = run(ctx)
	close(done)
	<-stopped
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if err != nil {
		return err
	}
	size, err = directorySize(stage)
	if err != nil {
		return err
	}
	if size > l.store.maxBytes {
		return ErrCapacity
	}
	return nil
}

func (l *Lease) publish(ctx context.Context, stage string, roots []string, exists bool) error {
	if !exists {
		size, err := directorySize(stage)
		if err != nil {
			return err
		}
		r, err := l.reserve(size)
		if err != nil {
			return err
		}
		defer r.release()
		return os.Rename(stage, l.Dir)
	}
	files, err := os.ReadDir(filepath.Join(stage, "objects", "pack"))
	if err != nil {
		return err
	}
	type install struct{ source, target string }
	var installs []install
	var additional int64
	for _, suffix := range []string{".pack", ".idx", ".rev"} {
		for _, file := range files {
			if !strings.HasSuffix(file.Name(), suffix) {
				continue
			}
			source := filepath.Join(stage, "objects", "pack", file.Name())
			target := filepath.Join(l.Dir, "objects", "pack", file.Name())
			info, err := file.Info()
			if err != nil {
				return err
			}
			additional += info.Size()
			old, err := os.Stat(target)
			if err == nil {
				additional -= old.Size()
			} else if !os.IsNotExist(err) {
				return err
			}
			installs = append(installs, install{source, target})
		}
	}
	var updates strings.Builder
	writes := make([]string, 0, len(roots))
	for _, root := range roots {
		fmt.Fprintf(&updates, "update refs/cache/roots/%s %s\n", root, root)
		writes = append(writes, "refs/cache/roots/"+root)
	}
	if updates.Len() > 0 {
		budget, err := refUpdateBudget(l.Dir, updates.Len())
		if err != nil {
			return err
		}
		additional += budget
	}
	r, err := l.reserve(additional)
	if err != nil {
		return err
	}
	defer r.release()
	for _, file := range installs {
		if err := os.Rename(file.source, file.target); err != nil {
			return err
		}
	}
	if err := clearStaleRefWrites(l.Dir, writes); err != nil {
		return err
	}
	if err := l.store.Git.Run(ctx, l.Dir, strings.NewReader(updates.String()), io.Discard, nil, "update-ref", "--stdin"); err != nil {
		return err
	}
	if !l.complete(ctx, roots, false) {
		return fmt.Errorf("published roots have incomplete dependencies")
	}
	return nil
}
