package cache

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

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

// rootsIndexMinScopeBytes gates the indexed completeness check by scope
// size. Consulting the index costs two git processes; the full walk it can
// replace costs one process plus a traversal of every object. Small scopes
// take the walk directly — measured on the benchmark fixture the index was
// a net pessimization — while large ones amortize the processes instantly.
const rootsIndexMinScopeBytes = 64 << 20

// Complete reports whether every object reachable from roots is already
// cached. Shared reader leases use it to decide whether a response can
// be generated without an exclusive lease. Large scopes first consult the
// pinned root index and fall back to the authoritative full walk.
func (l *Lease) Complete(ctx context.Context, roots []string) bool {
	if _, err := os.Stat(filepath.Join(l.Dir, "HEAD")); err != nil {
		return false
	}
	if l.indexedScope() && l.rootsCover(ctx, roots) {
		return true
	}
	return l.complete(ctx, roots, false)
}

// indexedScope reports whether the scope is large enough for the index to
// pay for its two git processes.
func (l *Lease) indexedScope() bool {
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	return l.entry.size >= rootsIndexMinScopeBytes
}

// rootsCover is the indexed completeness check. Refs under refs/cache/
// name closures that hydration verified complete — roots at publication,
// upstream refs only for objects whose closure passed the walk, retained
// refs preserving dropped roots with their objects. When every want is an
// ancestor of some indexed ref, the want's object closure is necessarily
// present and the full rev-list walk — a per-request traversal of the
// entire object graph on warm incremental fetches — can be skipped.
// Doubt (no refs, an error, a want outside every closure) falls back to
// the walk. The index is a hint, not proof: externally deleted packs, or
// a roots pin left by an interrupted publish, defeat it — but the shared
// generation then fails and the exclusive path's authoritative walk
// rehydrates, so the cache still self-heals within one request.
func (l *Lease) rootsCover(ctx context.Context, wants []string) bool {
	output, err := l.store.Git.Output(ctx, l.Dir, "", "for-each-ref", "--format=%(objectname)", "refs/cache/")
	if err != nil {
		return false
	}
	var in strings.Builder
	for _, want := range wants {
		in.WriteString(want)
		in.WriteString("\n")
	}
	indexed := false
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		oid := strings.TrimSpace(line)
		if oid == "" {
			continue
		}
		indexed = true
		in.WriteString("^")
		in.WriteString(oid)
		in.WriteString("\n")
	}
	if !indexed {
		return false
	}
	output, err = l.store.Git.Output(ctx, l.Dir, in.String(), "rev-list", "--max-count=1", "--stdin")
	return err == nil && len(output) == 0
}

// Ensure receives objects in a separate repository before publishing complete roots.
type HydrateSource func(context.Context, string, []string) error

func (l *Lease) Ensure(ctx context.Context, origin string, headers [][2]string, roots []string) (bool, error) {
	return l.EnsureWith(ctx, roots, func(ctx context.Context, stage string, roots []string) error {
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
		return l.store.Git.Run(ctx, stage, nil, io.Discard, nativegit.ConfigEnvironment(pairs), args...)
	})
}

func (l *Lease) EnsureWith(ctx context.Context, roots []string, source HydrateSource) (bool, error) {
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
	if err := g.InitBare(ctx, stage); err != nil {
		return false, err
	}
	if !exists {
		if err := atomicFile(stage, gcFile, []byte(fmt.Sprintf("%d\n", time.Now().Unix()))); err != nil {
			return false, err
		}
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
	if err := l.runBounded(ctx, stage, func(ctx context.Context) error { return source(ctx, stage, roots) }); err != nil {
		return false, err
	}
	if err := g.Run(ctx, stage, strings.NewReader(strings.Join(roots, "\n")+"\n"), io.Discard, nil, "rev-list", "--objects", "--missing=error", "--quiet", "--stdin"); err != nil {
		return false, err
	}
	if err := l.publish(ctx, stage, roots, exists); err != nil {
		return false, err
	}
	// Hydrations grow the root set; the count feeds the accumulation gate
	// that reconciles scopes whose upstream snapshot never drifts.
	l.store.mu.Lock()
	l.entry.hydrateSince++
	l.store.mu.Unlock()
	return false, nil
}
