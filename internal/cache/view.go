package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gitlab.com/banyan-vcs/git-cdn/internal/nativegit"
	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"
)

// AncestorsAll reports whether every candidate commit is an ancestor of
// (or equal to) at least one root.
func (l *Lease) AncestorsAll(ctx context.Context, candidates, roots []string) bool {
	within, err := l.AncestorsWithin(ctx, candidates, roots)
	return err == nil && len(within) == len(uniqueOIDs(candidates))
}

// AncestorsWithin returns the candidates that are existing commits and
// ancestors of (or equal to) at least one root; the others are absent from
// the result. Two git processes decide any number of candidates: a cat-file
// --batch-check gate keeps the commit-only rule (rev-list would silently
// ignore non-commit revisions), then one rev-list takes the commits as
// positive stdin revisions and the roots as negated ones and prints exactly
// the commits that escape the roots' histories. Callers that only need a
// yes/no use AncestorsAll; callers that can drop the uncovered candidates —
// client haves naming unpushed work, tags outside the fetched history —
// use this to keep the rest on the local path.
func (l *Lease) AncestorsWithin(ctx context.Context, candidates, roots []string) (map[string]bool, error) {
	candidates = uniqueOIDs(candidates)
	if len(candidates) == 0 {
		return nil, nil
	}
	if len(roots) == 0 {
		return nil, nil
	}
	output, err := l.store.Git.Output(ctx, l.Dir, strings.Join(candidates, "\n")+"\n", "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return nil, err
	}
	var commits []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid cached object response")
		}
		// Missing or non-commit candidates are never covered.
		if fields[1] == "commit" {
			commits = append(commits, fields[0])
		}
	}
	outside, err := l.commitsOutside(ctx, commits, roots)
	if err != nil {
		return nil, err
	}
	within := make(map[string]bool, len(commits))
	for _, commit := range commits {
		if !outside[commit] {
			within[commit] = true
		}
	}
	return within, nil
}

// commitsOutside returns the commits whose history is not contained in the
// union of the root histories. One rev-list answers for the whole batch; a
// batch whose escaping history overflows the metadata output limit is
// bisected until each part either fits or is a single commit — which is
// outside by construction, because a covered commit prints nothing.
func (l *Lease) commitsOutside(ctx context.Context, commits, roots []string) (map[string]bool, error) {
	if len(commits) == 0 {
		return nil, nil
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
	output, err := l.store.Git.Output(ctx, l.Dir, in.String(), "rev-list", "--stdin")
	if err == nil {
		outside := make(map[string]bool)
		for _, line := range strings.Fields(string(output)) {
			outside[line] = true
		}
		return outside, nil
	}
	if !errors.Is(err, nativegit.ErrOutputLimit) {
		return nil, err
	}
	if len(commits) == 1 {
		return map[string]bool{commits[0]: true}, nil
	}
	middle := len(commits) / 2
	left, err := l.commitsOutside(ctx, commits[:middle], roots)
	if err != nil {
		return nil, err
	}
	right, err := l.commitsOutside(ctx, commits[middle:], roots)
	if err != nil {
		return nil, err
	}
	for commit := range right {
		left[commit] = true
	}
	return left, nil
}

func uniqueOIDs(values []string) []string {
	if len(values) < 2 {
		return values
	}
	seen := make(map[string]bool, len(values))
	unique := values[:0]
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			unique = append(unique, value)
		}
	}
	return unique
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
