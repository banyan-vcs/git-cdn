package cache

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/chiyutianyi/git-cdn/internal/protocol"
)

func upstreamRef(name string) string {
	return "refs/cache/upstream/" + strings.TrimPrefix(name, "refs/")
}

// SyncRefs consumes the unprefixed, fresh upstream snapshot. Only complete
// objects already cached are retained; uncached branches are never hydrated by
// reconciliation. Storage refs remain separate from every client's ref view.
func (l *Lease) SyncRefs(ctx context.Context, snapshot protocol.Snapshot) error {
	if !snapshot.ObservedAt.IsZero() && snapshot.ObservedAt.Before(l.entry.refsObservedAt) {
		return nil
	}
	wanted := make(map[string]bool)
	for _, ref := range snapshot.Refs {
		wanted[ref.OID] = true
		if ref.Peeled != "" {
			wanted[ref.Peeled] = true
		}
	}
	var candidates []string
	for oid := range wanted {
		candidates = append(candidates, oid)
	}
	sort.Strings(candidates)
	present := make(map[string]bool)
	if len(candidates) > 0 {
		output, err := l.store.Git.Output(ctx, l.Dir, strings.Join(candidates, "\n")+"\n", "cat-file", "--batch-check=%(objectname) %(objecttype)")
		if err != nil {
			return err
		}
		var roots []string
		for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || !wanted[fields[0]] {
				return fmt.Errorf("invalid cached object response")
			}
			if fields[1] != "missing" {
				roots = append(roots, fields[0])
			}
		}
		if l.complete(ctx, roots, false) {
			for _, root := range roots {
				present[root] = true
			}
		} else {
			for _, root := range roots {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if l.complete(ctx, []string{root}, false) {
					present[root] = true
				}
			}
		}
	}
	desired := make(map[string]string)
	head := "ref: refs/cache/upstream/unborn\n"
	for _, ref := range snapshot.Refs {
		if ref.Name == "HEAD" {
			if strings.HasPrefix(ref.Symref, "refs/") {
				head = "ref: " + upstreamRef(ref.Symref) + "\n"
			} else if present[ref.OID] {
				head = ref.OID + "\n"
			}
		} else if present[ref.OID] {
			desired[upstreamRef(ref.Name)] = ref.OID
		}
	}
	for oid := range present {
		desired["refs/cache/roots/"+oid] = oid
	}
	current, err := l.store.Git.Output(ctx, l.Dir, "", "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return err
	}
	var updates strings.Builder
	var additional int64
	for _, line := range strings.Split(strings.TrimSpace(string(current)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return fmt.Errorf("invalid cached ref")
		}
		if oid, ok := desired[fields[0]]; ok {
			if oid == fields[1] {
				delete(desired, fields[0])
				continue
			}
		} else {
			fmt.Fprintf(&updates, "delete %s %s\n", fields[0], fields[1])
		}
	}
	var names []string
	for name := range desired {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&updates, "update %s %s\n", name, desired[name])
		additional += int64(2 * (len(name) + len(desired[name]) + 2))
	}
	oldHead, err := os.ReadFile(filepath.Join(l.Dir, "HEAD"))
	if err != nil {
		return err
	}
	// packed-refs deletion may rewrite the complete file and its lock.
	if info, err := os.Stat(filepath.Join(l.Dir, "packed-refs")); err == nil {
		additional += info.Size()
	} else if !os.IsNotExist(err) {
		return err
	}
	if string(oldHead) != head {
		additional += int64(len(head))
	}
	if updates.Len() == 0 && string(oldHead) == head {
		l.entry.refsObservedAt = snapshot.ObservedAt
		return nil
	}
	r, err := l.reserve(additional)
	if err != nil {
		return err
	}
	defer r.release()
	if updates.Len() > 0 {
		if err := l.store.Git.Run(ctx, l.Dir, strings.NewReader(updates.String()), io.Discard, nil, "update-ref", "--stdin"); err != nil {
			return err
		}
	}
	if string(oldHead) != head {
		if err := os.WriteFile(filepath.Join(l.Dir, "HEAD.lock"), []byte(head), 0600); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(l.Dir, "HEAD.lock"), filepath.Join(l.Dir, "HEAD")); err != nil {
			return err
		}
	}
	l.entry.refsObservedAt = snapshot.ObservedAt
	return nil
}
