package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	// Roots leaving the advertised set retire into the grace period; the
	// count feeds the retirement metric.
	var retired int
	retained, err := readRetention(l.Dir)
	if err != nil {
		return err
	}
	for oid := range retained {
		if present[oid] {
			delete(retained, oid)
		} else {
			desired[retainedPrefix+oid] = oid
		}
	}
	// Preserve dropped root closures independently of the upstream ref names.
	for _, line := range strings.Split(strings.TrimSpace(string(current)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return fmt.Errorf("invalid cached ref")
		}
		if present[fields[1]] {
			continue
		}
		if strings.HasPrefix(fields[0], retainedPrefix) || strings.HasPrefix(fields[0], "refs/cache/roots/") || strings.HasPrefix(fields[0], "refs/cache/upstream/") {
			if retained[fields[1]] == 0 {
				retained[fields[1]] = time.Now().UnixNano()
			}
			desired[retainedPrefix+fields[1]] = fields[1]
			if strings.HasPrefix(fields[0], "refs/cache/roots/") {
				retired++
			}
		}
	}
	retentionData, err := json.Marshal(retained)
	if err != nil {
		return err
	}
	previousRetention, err := os.ReadFile(filepath.Join(l.Dir, retentionFile))
	if err != nil && !os.IsNotExist(err) {
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
	}
	oldHead, err := l.store.Git.Head(ctx, l.Dir)
	if err != nil {
		return err
	}
	retentionChanged := string(previousRetention) != string(retentionData)
	if retentionChanged {
		additional += int64(len(retentionData))
	}
	if updates.Len() == 0 && oldHead == head && !retentionChanged {
		l.entry.refsObservedAt = snapshot.ObservedAt
		l.recordReconcile(snapshot, retired)
		return nil
	}
	if updates.Len() > 0 || oldHead != head {
		recordBytes := updates.Len()
		if oldHead != head {
			recordBytes += len(head)
		}
		budget, err := refUpdateBudget(l.Dir, recordBytes)
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
	// Persist timestamps before installing grace refs; interrupted updates can
	// never create a grace ref with an accidentally old/missing age.
	if retentionChanged {
		if err := atomicFile(l.Dir, retentionFile, retentionData); err != nil {
			return err
		}
	}
	if updates.Len() > 0 {
		if err := l.store.Git.Run(ctx, l.Dir, strings.NewReader(updates.String()), io.Discard, nil, "update-ref", "--stdin"); err != nil {
			return err
		}
	}
	if oldHead != head {
		if err := l.store.Git.SetHead(ctx, l.Dir, head); err != nil {
			return err
		}
	}
	l.entry.refsObservedAt = snapshot.ObservedAt
	l.recordReconcile(snapshot, retired)
	return nil
}

// recordReconcile marks the snapshot as applied for trigger debouncing and
// counts the roots retired into the retention grace period. The caller has
// finished its ref transaction; this takes the store lock briefly.
func (l *Lease) recordReconcile(snapshot protocol.Snapshot, retired int) {
	l.store.mu.Lock()
	l.entry.reconcileFP = snapshot.Fingerprint()
	l.entry.hydrateSince = 0
	l.store.reconciles++
	l.store.rootsRetired += uint64(retired)
	l.store.mu.Unlock()
}
