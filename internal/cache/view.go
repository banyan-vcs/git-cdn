package cache

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/chiyutianyi/git-cdn/internal/protocol"
)

func (l *Lease) Ancestor(ctx context.Context, have string, wants []string) bool {
	typeName, err := l.store.Git.Output(ctx, l.Dir, "", "cat-file", "-t", have)
	if err != nil || string(typeName) != "commit\n" {
		return false
	}
	for _, want := range wants {
		if err := l.store.Git.Run(ctx, l.Dir, nil, io.Discard, nil, "merge-base", "--is-ancestor", have, want+"^{commit}"); err == nil {
			return true
		}
	}
	return false
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
	if err := g.Run(ctx, "", nil, io.Discard, nil, "init", "--quiet", "--bare", "--object-format=sha1", view); err != nil {
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
				if err := g.Run(ctx, view, nil, io.Discard, nil, "symbolic-ref", "HEAD", ref.Symref); err != nil {
					return "", err
				}
			} else if allowed[ref.OID] {
				if err := os.WriteFile(filepath.Join(view, "HEAD"), []byte(ref.OID+"\n"), 0600); err != nil {
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
