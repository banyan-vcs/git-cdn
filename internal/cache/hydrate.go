package cache

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/chiyutianyi/git-cdn/internal/nativegit"
)

func (l *Lease) complete(ctx context.Context, roots []string, all bool) bool {
	args := []string{"rev-list", "--objects", "--missing=error", "--quiet"}
	if all {
		args = append(args, "--all")
	} else {
		args = append(args, "--stdin")
	}
	err := l.store.Git.Run(ctx, l.Dir, strings.NewReader(strings.Join(roots, "\n")+"\n"), io.Discard, nil, args...)
	return err == nil
}

// Ensure receives objects in a separate repository before publishing complete roots.
func (l *Lease) Ensure(ctx context.Context, origin string, headers [][2]string, roots []string) (bool, error) {
	g := l.store.Git
	if err := l.store.ensureScope(l.entry.key); err != nil {
		return false, err
	}
	_, existsErr := os.Stat(filepath.Join(l.Dir, "HEAD"))
	exists := existsErr == nil
	if exists && l.complete(ctx, roots, false) {
		return true, nil
	}
	stage, err := os.MkdirTemp(l.store.Tmp, "hydrate-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(stage)
	if err := g.Run(ctx, "", nil, io.Discard, nil, "init", "--quiet", "--bare", "--object-format=sha1", stage); err != nil {
		return false, err
	}
	// Use previous roots for negotiation only if their dependency graph is intact.
	if exists && l.complete(ctx, nil, true) {
		objects := filepath.Join(l.Dir, "objects")
		if err := os.WriteFile(filepath.Join(stage, "objects", "info", "alternates"), []byte(objects+"\n"), 0600); err != nil {
			return false, err
		}
		refs, err := g.Output(ctx, l.Dir, "", "for-each-ref", "--format=update %(refname) %(objectname)", "refs/cache/roots/")
		if err != nil {
			return false, err
		}
		if len(refs) > 0 {
			if err := g.Run(ctx, stage, strings.NewReader(string(refs)), io.Discard, nil, "update-ref", "--stdin"); err != nil {
				return false, err
			}
		}
	}
	pairs := [][2]string{
		{"credential.helper", ""}, {"credential.interactive", "false"},
		{"http.proxy", ""}, {"http.followRedirects", "false"},
		{"http.sslVerify", "true"}, {"http.extraHeader", ""},
		{"fetch.fsckObjects", "true"}, {"core.hooksPath", os.DevNull},
		{"fetch.unpackLimit", "0"}, {"transfer.unpackLimit", "0"},
	}
	for _, h := range headers {
		pairs = append(pairs, [2]string{"http." + strings.TrimSuffix(origin, "/") + "/.extraHeader", h[0] + ": " + h[1]})
	}
	args := []string{"-c", "protocol.version=2", "fetch", "--no-tags", "--no-recurse-submodules", "--no-auto-maintenance", "--no-write-fetch-head", origin}
	for _, root := range roots {
		args = append(args, root+":refs/cache/roots/"+root)
	}
	if err := l.fetchBounded(ctx, stage, nativegit.ConfigEnvironment(pairs), args...); err != nil {
		return false, err
	}
	if err := g.Run(ctx, stage, strings.NewReader(strings.Join(roots, "\n")+"\n"), io.Discard, nil, "rev-list", "--objects", "--missing=error", "--quiet", "--stdin"); err != nil {
		return false, err
	}
	if err := l.publish(ctx, stage, roots, exists); err != nil {
		return false, err
	}
	return false, nil
}
