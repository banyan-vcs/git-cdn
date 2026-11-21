package cache

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"
)

// AncestorsAll reports whether every candidate commit is an ancestor of
// (or equal to) at least one root. Two git processes decide the whole
// batch: a cat-file --batch-check gate keeps the earlier commit-only rule
// (rev-list would silently ignore non-commit revisions), then one
// rev-list takes the candidates as positive stdin revisions and the roots
// as explicitly negated ones — empty output means every candidate's
// history is contained in the union of the root histories, which holds
// exactly when each candidate is an ancestor of some root. Replacing the
// per-pair cat-file/merge-base probes drops a warm incremental request
// from hundreds of serialized git spawns to two.
func (l *Lease) AncestorsAll(ctx context.Context, candidates, roots []string) bool {
	if len(candidates) == 0 {
		return true
	}
	if len(roots) == 0 {
		return false
	}
	output, err := l.store.Git.Output(ctx, l.Dir, strings.Join(candidates, "\n")+"\n", "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return false
	}
	commits := make([]string, 0, len(candidates))
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != "commit" {
			// Missing or non-commit candidates were never covered.
			return false
		}
		commits = append(commits, fields[0])
	}
	var in strings.Builder
	for _, commit := range commits {
		in.WriteString(commit)
		in.WriteString("\n")
	}
	for _, root := range roots {
		in.WriteString("^")
		in.WriteString(root)
		in.WriteString("\n")
	}
	output, err = l.store.Git.Output(ctx, l.Dir, in.String(), "rev-list", "--max-count=1", "--stdin")
	return err == nil && len(output) == 0
}

func (l *Lease) View(ctx context.Context, snapshot protocol.Snapshot, roots []string) (string, error) {
	view, err := os.MkdirTemp(l.store.Tmp, "view-")
	if err != nil {
		return "", err
	}
	failed := true
	defer func() {
		if failed {
			os.RemoveAll(view)
		}
	}()
	g := l.store.Git
	if err := g.InitBare(ctx, view); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(view, "objects", "info", "alternates"), []byte(filepath.Join(l.Dir, "objects")+"\n"), 0600); err != nil {
		return "", err
	}
	allowed := make(map[string]bool)
	for _, root := range roots {
		allowed[root] = true
	}
	var updates strings.Builder
	for _, ref := range snapshot.Refs {
		if ref.Name == "HEAD" {
			if ref.Symref != "" {
				if err := g.SetHead(ctx, view, "ref: "+ref.Symref+"\n"); err != nil {
					return "", err
				}
			} else if allowed[ref.OID] {
				if err := g.SetHead(ctx, view, ref.OID+"\n"); err != nil {
					return "", err
				}
			}
		} else if allowed[ref.OID] {
			fmt.Fprintf(&updates, "update %s %s\n", ref.Name, ref.OID)
		}
	}
	if updates.Len() > 0 {
		if err := g.Run(ctx, view, strings.NewReader(updates.String()), io.Discard, nil, "update-ref", "--stdin"); err != nil {
			return "", err
		}
	}
	failed = false
	return view, nil
}
