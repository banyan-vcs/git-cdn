// Exercise a public GitHub repository through Git CDN without pushing remote refs.
package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/scripts/internal/testutil"
)

type options struct {
	base, repository, binary, output string
	origin                           string
	concurrency, timeout             int
}

type commandRecord struct {
	Label    string   `json:"label"`
	Argv     []string `json:"argv"`
	ExitCode int      `json:"exit_code"`
	Seconds  float64  `json:"seconds"`
	Statuses []string `json:"cache_statuses"`
	Trace    string   `json:"trace"`
}

type suite struct {
	args                   options
	url, branch, head, old string
	ctx                    context.Context
	mu                     sync.Mutex
	next                   int
	commands               []commandRecord
	cases                  []map[string]any
	report                 map[string]any
	client                 *http.Client
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func check(ok bool, message string) {
	if !ok {
		panic(message)
	}
}

func rounded(d time.Duration) float64 { return float64(d.Milliseconds()) / 1000 }

var cacheHeader = regexp.MustCompile(`(?i)Recv header: X-GitCDN-Cache-Status: (\S+)`)

func (s *suite) git(label, dir string, lazy, check bool, args ...string) ([]byte, commandRecord) {
	s.mu.Lock()
	s.next++
	name := fmt.Sprintf("%03d-%s", s.next, label)
	s.mu.Unlock()
	trace := filepath.Join("logs", name+".curl.log")
	ctx, cancel := context.WithTimeout(s.ctx, time.Duration(s.args.timeout)*time.Second)
	defer cancel()
	argv := append([]string{"-c", "credential.helper=", "-c", "protocol.version=2"}, args...)
	cmd := testutil.Command(ctx, "git", argv...)
	if dir == "" {
		dir = s.args.output
	}
	cmd.Dir = dir
	mode := "1"
	if lazy {
		mode = "0"
	}
	cmd.Env = append(testutil.Environment(), "GIT_NO_LAZY_FETCH="+mode,
		"GIT_TRACE_CURL="+filepath.Join(s.args.output, trace), "GIT_TRACE_CURL_NO_DATA=1",
		"GIT_TRACE_PACKET="+filepath.Join(s.args.output, "logs", name+".packet.log"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	code := 0
	if err != nil {
		code = -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
	}
	must(os.WriteFile(filepath.Join(s.args.output, "logs", name+".stderr.log"), stderr.Bytes(), 0600))
	data, readErr := os.ReadFile(filepath.Join(s.args.output, trace))
	if readErr != nil && !os.IsNotExist(readErr) {
		must(readErr)
	}
	statuses := []string{}
	for _, match := range cacheHeader.FindAllSubmatch(data, -1) {
		statuses = append(statuses, string(match[1]))
	}
	record := commandRecord{label, append([]string{"git"}, argv...), code, rounded(time.Since(start)), statuses, filepath.ToSlash(trace)}
	s.mu.Lock()
	s.commands = append(s.commands, record)
	s.mu.Unlock()
	if ctx.Err() != nil {
		panic(fmt.Errorf("%s: %w", label, ctx.Err()))
	}
	if check && err != nil {
		message := stderr.String()
		if len(message) > 2000 {
			message = message[len(message)-2000:]
		}
		panic(fmt.Errorf("%s: %w: %s", label, err, message))
	}
	return stdout.Bytes(), record
}

func (s *suite) text(label, dir string, args ...string) string {
	out, _ := s.git(label, dir, false, true, args...)
	return strings.TrimSpace(string(out))
}

func (s *suite) get(base, path string) []byte {
	request, err := http.NewRequestWithContext(s.ctx, http.MethodGet, strings.TrimRight(base, "/")+path, nil)
	must(err)
	response, err := s.client.Do(request)
	must(err)
	defer response.Body.Close()
	check(response.StatusCode == http.StatusOK, fmt.Sprintf("%s: HTTP %d", path, response.StatusCode))
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	must(err)
	return data
}

// metrics reads /metrics into samples named with their labels. The
// counters are integers; the one ratio is not.
func (s *suite) metrics(base string) map[string]float64 {
	metrics := map[string]float64{}
	for _, line := range strings.Split(string(s.get(base, "/metrics")), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		check(len(fields) == 2, "invalid metrics line")
		value, err := strconv.ParseFloat(fields[1], 64)
		must(err)
		metrics[fields[0]] = value
	}
	return metrics
}

func (s *suite) missing(repo string) map[string]bool {
	missing := map[string]bool{}
	for _, line := range strings.Split(s.text("missing-objects", repo, "rev-list", "--objects", "--missing=print", "HEAD"), "\n") {
		if strings.HasPrefix(line, "?") {
			missing[line[1:]] = true
		}
	}
	return missing
}

func (s *suite) tip(repo, ref string) string { return s.text("tip", repo, "rev-parse", ref) }

func (s *suite) verify(repo, kind string) map[string]any {
	check(s.tip(repo, "HEAD") == s.head, "unexpected HEAD in "+filepath.Base(repo))
	expected := strconv.FormatBool(kind == "shallow")
	check(s.text("is-shallow", repo, "rev-parse", "--is-shallow-repository") == expected, "wrong shallow state")
	s.git("connectivity", repo, false, true, "fsck", "--connectivity-only", "--no-dangling")
	details := map[string]any{"head": s.head}
	missing := s.missing(repo)
	if kind == "partial" {
		check(s.text("promisor", repo, "config", "--get", "remote.origin.promisor") == "true", "not a promisor repository")
		check(s.text("filter", repo, "config", "--get", "remote.origin.partialclonefilter") == "blob:none", "partial filter lost")
		check(len(missing) > 0, "blob:none left no promised blobs missing")
		details["missing_objects"], details["promisor"] = len(missing), true
	} else {
		check(len(missing) == 0, "unexpected missing objects in complete/shallow closure")
	}
	return details
}

func lastStatus(record commandRecord) string {
	if len(record.Statuses) == 0 {
		return ""
	}
	return record.Statuses[len(record.Statuses)-1]
}

func (s *suite) clone(label, kind, url string) (string, map[string]any) {
	repo := filepath.Join(s.args.output, label)
	args := []string{"clone", "--no-checkout", "--no-tags", "--single-branch", "--branch", s.branch}
	switch kind {
	case "partial":
		args = append(args, "--filter=blob:none")
	case "shallow":
		args = append(args, "--depth", "1")
	}
	if url == "" {
		url = s.url
	}
	_, record := s.git(label, "", false, true, append(args, url, repo)...)
	details := s.verify(repo, kind)
	details["kind"], details["seconds"], details["cache_statuses"] = kind, record.Seconds, record.Statuses
	return repo, details
}

func capture(function func() map[string]any) (details map[string]any, err error) {
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("%v", value)
		}
	}()
	return function(), nil
}

func (s *suite) save() {
	s.mu.Lock()
	commands := append([]commandRecord(nil), s.commands...)
	s.mu.Unlock()
	sort.Slice(commands, func(i, j int) bool { return commands[i].Trace < commands[j].Trace })
	s.report["commands"], s.report["cases"] = commands, s.cases
	data, err := json.MarshalIndent(s.report, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(s.args.output, "results.json"), append(data, '\n'), 0600))
}

func (s *suite) runCase(name string, function func() map[string]any) {
	start := time.Now()
	details, err := capture(function)
	if details == nil {
		details = map[string]any{}
	}
	details["name"], details["seconds"], details["passed"] = name, rounded(time.Since(start)), err == nil
	if err != nil {
		details["error"] = err.Error()
		fmt.Println("FAIL", name, err)
	} else {
		fmt.Println("PASS", name)
	}
	s.cases = append(s.cases, details)
	s.save()
}

func (s *suite) discover() {
	s.report["git_version"], s.report["metrics_before"] = s.text("version", "", "--version"), s.metrics(s.args.base)
	refs := s.text("discover", "", "ls-remote", "--symref", s.url, "HEAD", "refs/heads/*")
	branch := regexp.MustCompile(`ref: refs/heads/(\S+)\s+HEAD`).FindStringSubmatch(refs)
	head := regexp.MustCompile(`(?m)^([0-9a-f]{40})\s+HEAD$`).FindStringSubmatch(refs)
	check(len(branch) == 2 && len(head) == 2, "missing SHA-1 default branch in discovery")
	s.branch, s.head = branch[1], head[1]
	seed := filepath.Join(s.args.output, "seed")
	s.git("history-seed", "", false, true, "clone", "--no-checkout", "--no-tags", "--single-branch", "--depth", "16", "--branch", s.branch, s.url, seed)
	check(s.tip(seed, "HEAD") == s.head, "upstream HEAD moved during discovery; rerun for a consistent snapshot")
	commits := strings.Fields(s.text("baseline", seed, "rev-list", "--first-parent", "--max-count=8", "HEAD"))
	check(len(commits) > 1, "repository has insufficient update history")
	index := len(commits) - 1
	if index > 5 {
		index = 5
	}
	s.old = commits[index]
	s.report["branch"], s.report["head"], s.report["baseline_commit"] = s.branch, s.head, s.old
}

func (s *suite) lazyBlob(repo string, changed map[string]bool) map[string]any {
	missing := s.missing(repo)
	// -z avoids Git's quoted filename format for spaces and non-ASCII paths.
	data, _ := s.git("tree", repo, false, true, "ls-tree", "-rz", "HEAD")
	type entry struct{ path, oid string }
	var candidates []entry
	for _, row := range bytes.Split(data, []byte{0}) {
		if len(row) == 0 {
			continue
		}
		parts := bytes.SplitN(row, []byte{'\t'}, 2)
		check(len(parts) == 2, "invalid tree entry")
		fields, path := strings.Fields(string(parts[0])), string(parts[1])
		if len(fields) == 3 && fields[1] == "blob" && missing[fields[2]] && (changed == nil || changed[path]) {
			candidates = append(candidates, entry{path, fields[2]})
		}
	}
	check(len(candidates) > 0, "no missing working-tree blob to test")
	sort.Slice(candidates, func(i, j int) bool {
		a, b := strings.EqualFold(candidates[i].path, "readme.md"), strings.EqualFold(candidates[j].path, "readme.md")
		if a != b {
			return a
		}
		return candidates[i].path < candidates[j].path
	})
	chosen := candidates[0]
	content, record := s.git("lazy-blob", repo, true, true, "show", "HEAD:"+chosen.path)
	hash := sha1.New()
	fmt.Fprintf(hash, "blob %d%c", len(content), 0)
	hash.Write(content)
	check(fmt.Sprintf("%x", hash.Sum(nil)) == chosen.oid, "lazy blob hash differs from tree")
	check(len(s.missing(repo)) < len(missing), "lazy fetch did not fill missing blobs")
	return map[string]any{"path": chosen.path, "blob_oid": chosen.oid, "bytes": len(content), "cache_statuses": record.Statuses}
}

func (s *suite) prepareUpdate(label, kind string) string {
	repo := filepath.Join(s.args.output, label)
	s.git(label+"-init", "", false, true, "init", "--quiet", repo)
	s.git(label+"-remote", repo, false, true, "remote", "add", "origin", s.url)
	args := []string{"fetch", "--no-tags"}
	if kind == "partial" {
		s.git(label+"-promisor", repo, false, true, "config", "remote.origin.promisor", "true")
		s.git(label+"-filter", repo, false, true, "config", "remote.origin.partialclonefilter", "blob:none")
		args = append(args, "--filter=blob:none")
	} else if kind == "shallow" {
		args = append(args, "--depth", "1")
	}
	s.git(label+"-old", repo, false, true, append(args, "origin", s.old)...)
	s.git(label+"-root", repo, false, true, "update-ref", "refs/heads/baseline", "FETCH_HEAD")
	s.git(label+"-head", repo, false, true, "symbolic-ref", "HEAD", "refs/heads/baseline")
	check(s.tip(repo, "HEAD") == s.old, "historical baseline was not fetched")
	return repo
}

func (s *suite) update(label, kind, repo string) map[string]any {
	if repo == "" {
		repo = s.prepareUpdate(label, kind)
	}
	args := []string{"fetch", "--no-tags"}
	if kind == "partial" {
		args = append(args, "--filter=blob:none")
	} else if kind == "shallow" {
		args = append(args, "--depth", "1")
	}
	_, record := s.git(label+"-new", repo, false, true, append(args, "origin", "+refs/heads/"+s.branch+":refs/remotes/origin/"+s.branch)...)
	newTip := s.tip(repo, "refs/remotes/origin/"+s.branch)
	check(newTip == s.head && newTip != s.old, "update did not reach sampled current branch")
	s.git(label+"-advance", repo, false, true, "update-ref", "refs/heads/baseline", newTip)
	details := s.verify(repo, kind)
	if kind != "shallow" {
		count, err := strconv.Atoi(s.text(label+"-count", repo, "rev-list", "--count", s.old+".."+newTip))
		must(err)
		check(count > 0, "update transferred no new commits")
		details["new_commits"] = count
	}
	if kind == "full" {
		check(lastStatus(record) == "HIT", "normal update missed warm local cache")
	}
	if kind == "partial" {
		data, _ := s.git(label+"-changed", repo, false, true, "diff-tree", "--no-renames", "--no-commit-id", "--name-only", "-r", "-z", s.old, newTip)
		changed := map[string]bool{}
		for _, path := range bytes.Split(data, []byte{0}) {
			if len(path) > 0 {
				changed[string(path)] = true
			}
		}
		details["changed_blob"] = s.lazyBlob(repo, changed)
	} else {
		s.git(label+"-checkout", repo, false, true, "checkout", "--force", "baseline")
		check(s.text(label+"-worktree", repo, "status", "--porcelain") == "", "updated worktree is not clean")
		details["working_tree_clean"] = true
	}
	details["kind"], details["old"], details["new"] = kind, s.old, newTip
	details["cache_statuses"], details["fetch_seconds"] = record.Statuses, record.Seconds
	return details
}

func parallel(count int, work func(int) map[string]any) map[string]any {
	results, failures := make([]map[string]any, count), make([]error, count)
	var group sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < count; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			results[index], failures[index] = capture(func() map[string]any { return work(index) })
		}(i)
	}
	close(start)
	group.Wait()
	for i, err := range failures {
		if err != nil {
			panic(fmt.Errorf("worker %d: %w", i, err))
		}
	}
	return map[string]any{"workers": results, "all_succeeded": true}
}

func (s *suite) dedicatedServer(name, origin string) (base string, stop func()) {
	config := fmt.Sprintf("mode: multi\nlisten: 127.0.0.1:0\ncache_dir: %q\nupstreams:\n  github:\n    url: %q\n", filepath.Join(s.args.output, name+"-cache"), origin)
	configPath := filepath.Join(s.args.output, name+".yaml")
	must(os.WriteFile(configPath, []byte(config), 0600))
	logPath := filepath.Join(s.args.output, name+".log")
	log, err := os.Create(logPath)
	must(err)
	ctx, cancel := context.WithCancel(s.ctx)
	cmd := testutil.Command(ctx, s.args.binary, "-config", configPath)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		cancel()
		log.Close()
		panic(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	stop = func() {
		defer log.Close()
		defer cancel()
		cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			cancel()
			<-done
		}
	}
	success := false
	defer func() {
		if !success {
			stop()
		}
	}()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			data, _ := os.ReadFile(logPath)
			panic("dedicated server exited: " + string(data))
		case <-timer.C:
			panic("dedicated server startup timed out")
		case <-ctx.Done():
			panic(ctx.Err())
		case <-ticker.C:
			data, err := os.ReadFile(logPath)
			must(err)
			for _, line := range bytes.Split(data, []byte{'\n'}) {
				var event struct {
					Message string `json:"msg"`
					Address string `json:"address"`
				}
				if json.Unmarshal(line, &event) == nil && event.Message == "git CDN listening" && event.Address != "" {
					success = true
					return "http://" + event.Address, stop
				}
			}
		}
	}
}

func cacheRepo(root string) string {
	var repos []string
	must(filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == "repo.git" {
			repos = append(repos, path)
			return filepath.SkipDir
		}
		return nil
	}))
	check(len(repos) == 1, "unexpected cache scope count")
	return repos[0]
}

func (s *suite) coldCache() map[string]any {
	base, stop := s.dedicatedServer("cold-server", s.args.origin)
	defer stop()
	before := s.metrics(base)
	results := parallel(s.args.concurrency, func(index int) map[string]any {
		_, details := s.clone(fmt.Sprintf("cold-clone-%d", index), "full", base+"/github/"+s.args.repository)
		return details
	})
	after := s.metrics(base)
	check(after["gitcdn_hydrations_total"] == 1, "cold burst did not share one hydration")
	check(after["gitcdn_local_requests_total"] == float64(s.args.concurrency), "cold requests did not all use local upload-pack")
	misses := 0
	for _, worker := range results["workers"].([]map[string]any) {
		statuses := worker["cache_statuses"].([]string)
		if len(statuses) > 0 && statuses[len(statuses)-1] == "MISS" {
			misses++
		}
	}
	check(misses == 1, "expected one MISS followed by HITs")
	cached := cacheRepo(filepath.Join(s.args.output, "cold-server-cache", "repos"))
	s.git("cold-cache-closure", cached, false, true, "rev-list", "--objects", "--missing=error", "--quiet", s.head)
	check(s.text("cold-cache-shallow", cached, "rev-parse", "--is-shallow-repository") == "false", "server stored a shallow cache")
	results["metrics_before"], results["metrics_after"] = before, after
	results["server_cache_complete"], results["server_cache_shallow"] = true, false
	return results
}

func packSizes(repo string) map[string]int64 {
	files, err := filepath.Glob(filepath.Join(repo, "objects", "pack", "*.pack"))
	must(err)
	sizes := map[string]int64{}
	for _, path := range files {
		info, err := os.Stat(path)
		must(err)
		sizes[filepath.Base(path)] = info.Size()
	}
	return sizes
}

func (s *suite) replayUpdate() map[string]any {
	root := filepath.Join(s.args.output, "replay-origin")
	originRepo := filepath.Join(root, filepath.FromSlash(s.args.repository)+".git")
	must(os.MkdirAll(filepath.Dir(originRepo), 0700))
	s.git("replay-origin", "", false, true, "clone", "--bare", "--no-local", filepath.Join(s.args.output, "full"), originRepo)
	s.git("replay-old-ref", originRepo, false, true, "update-ref", "refs/heads/"+s.branch, s.old)
	var fetches atomic.Int64
	origin := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
		if err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		if bytes.Contains(body, []byte("command=fetch")) {
			fetches.Add(1)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		cmd := testutil.Command(ctx, "git", "http-backend")
		cmd.Env = append(testutil.Environment(), "GIT_PROJECT_ROOT="+root, "GIT_HTTP_EXPORT_ALL=1",
			"PATH_INFO="+r.URL.Path, "QUERY_STRING="+r.URL.RawQuery, "REQUEST_METHOD="+r.Method,
			"CONTENT_TYPE="+r.Header.Get("Content-Type"), "CONTENT_LENGTH="+strconv.Itoa(len(body)),
			"SERVER_PROTOCOL=HTTP/1.1", "HTTP_GIT_PROTOCOL="+r.Header.Get("Git-Protocol"))
		cmd.Stdin = bytes.NewReader(body)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if cmd.Run() != nil {
			http.Error(w, "origin failed", 500)
			return
		}
		headers, payload, ok := bytes.Cut(out.Bytes(), []byte("\r\n\r\n"))
		if !ok {
			headers, payload, ok = bytes.Cut(out.Bytes(), []byte("\n\n"))
		}
		if !ok {
			http.Error(w, "invalid CGI output", 500)
			return
		}
		status := http.StatusOK
		for _, line := range strings.Split(string(headers), "\n") {
			key, value, valid := strings.Cut(line, ":")
			if !valid {
				http.Error(w, "invalid CGI header", 500)
				return
			}
			value = strings.TrimSpace(value)
			if strings.EqualFold(key, "Status") {
				fields := strings.Fields(value)
				if len(fields) == 0 {
					http.Error(w, "invalid CGI status", 500)
					return
				}
				status, err = strconv.Atoi(fields[0])
				if err != nil {
					http.Error(w, "invalid CGI status", 500)
					return
				}
			} else {
				w.Header().Add(key, value)
			}
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(status)
		w.Write(payload)
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	go origin.Serve(listener)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if origin.Shutdown(ctx) != nil {
			origin.Close()
		}
	}()
	base, stop := s.dedicatedServer("replay-server", "http://"+listener.Addr().String())
	defer stop()
	url := base + "/github/" + s.args.repository
	client := filepath.Join(s.args.output, "replay-client")
	_, cold := s.git("replay-clone-old", "", false, true, "clone", "--no-checkout", "--no-tags", "--single-branch", url, client)
	check(s.tip(client, "HEAD") == s.old && lastStatus(cold) == "MISS", "old snapshot did not populate cache")
	cached := cacheRepo(filepath.Join(s.args.output, "replay-server-cache", "repos"))
	before := packSizes(cached)
	_, absent := s.git("replay-new-missing", cached, false, false, "cat-file", "-e", s.head)
	check(absent.ExitCode != 0, "new root already present before update")
	s.git("replay-new-ref", originRepo, false, true, "update-ref", "refs/heads/"+s.branch, s.head)
	_, update := s.git("replay-fetch-new", client, false, true, "fetch", "--no-tags", "origin")
	check(s.tip(client, "origin/"+s.branch) == s.head && lastStatus(update) == "MISS", "new snapshot did not trigger hydration")
	s.git("replay-checkout-new", client, false, true, "checkout", "--force", "-B", s.branch, "origin/"+s.branch)
	s.verify(client, "full")
	warm := filepath.Join(s.args.output, "replay-warm")
	_, repeat := s.git("replay-warm", "", false, true, "clone", "--no-checkout", "--no-tags", "--single-branch", url+".git", warm)
	s.verify(warm, "full")
	check(lastStatus(repeat) == "HIT", "updated cache was not reused")
	metrics := s.metrics(base)
	var initial, added int64
	for _, size := range before {
		initial += size
	}
	for name, size := range packSizes(cached) {
		if _, ok := before[name]; !ok {
			added += size
		}
	}
	check(metrics["gitcdn_hydrations_total"] == 2 && fetches.Load() == 2, "update/warm clone performed extra origin fetches")
	check(added > 0, "update published no new object data")
	return map[string]any{"origin_kind": "controlled local HTTP origin using actual GitHub repository objects", "old": s.old, "new": s.head,
		"new_missing_before_update": true, "initial_pack_bytes": initial, "incremental_pack_bytes": added,
		"origin_fetches": fetches.Load(), "metrics_after": metrics, "cache_statuses": []string{lastStatus(cold), lastStatus(update), lastStatus(repeat)}}
}

func (s *suite) run() bool {
	s.runCase("discovery", func() map[string]any { s.discover(); return nil })
	if s.head == "" || s.old == "" {
		return false
	}
	partial, shallow := "", ""
	s.runCase("partial_clone_blob_none", func() map[string]any {
		repo, details := s.clone("partial", "partial", "")
		partial = repo
		statuses := details["cache_statuses"].([]string)
		check(len(statuses) > 0 && (statuses[len(statuses)-1] == "MISS" || statuses[len(statuses)-1] == "HIT"), "partial clone missed local serving")
		return details
	})
	s.runCase("partial_lazy_blob", func() map[string]any { check(partial != "", "partial clone failed"); return s.lazyBlob(partial, nil) })
	s.runCase("partial_checkout", func() map[string]any {
		check(partial != "", "partial clone failed")
		_, record := s.git("partial-checkout", partial, true, true, "checkout", "--force", s.branch)
		check(s.text("worktree", partial, "status", "--porcelain") == "", "partial checkout produced dirty worktree")
		return map[string]any{"cache_statuses": record.Statuses, "historical_missing_objects": len(s.missing(partial))}
	})
	s.runCase("shallow_clone_depth_1", func() map[string]any {
		repo, details := s.clone("shallow", "shallow", "")
		shallow = repo
		check(s.text("shallow-count", repo, "rev-list", "--count", "HEAD") == "1", "depth=1 has unexpected history")
		details["commits"] = 1
		return details
	})
	s.runCase("shallow_deepen", func() map[string]any {
		check(shallow != "", "shallow clone failed")
		_, record := s.git("deepen", shallow, false, true, "fetch", "--deepen=5", "origin", s.branch)
		count, err := strconv.Atoi(s.text("deepened-count", shallow, "rev-list", "--count", "HEAD"))
		must(err)
		check(count >= 6, "deepen did not extend history")
		return map[string]any{"commits": count, "cache_statuses": record.Statuses}
	})
	s.runCase("shallow_unshallow", func() map[string]any {
		check(shallow != "", "shallow clone failed")
		_, record := s.git("unshallow", shallow, false, true, "fetch", "--unshallow", "origin", s.branch)
		details := s.verify(shallow, "full")
		details["cache_statuses"] = record.Statuses
		return details
	})
	s.runCase("ordinary_clone_local", func() map[string]any {
		_, details := s.clone("full", "full", "")
		statuses := details["cache_statuses"].([]string)
		check(len(statuses) > 0 && (statuses[len(statuses)-1] == "HIT" || statuses[len(statuses)-1] == "MISS"), "ordinary clone fell back")
		return details
	})
	s.runCase("ordinary_clone_warm", func() map[string]any {
		_, details := s.clone("full-warm", "full", s.url+".git")
		statuses := details["cache_statuses"].([]string)
		check(len(statuses) > 0 && statuses[len(statuses)-1] == "HIT", "repeat clone did not reuse cache")
		return details
	})
	for _, kind := range []string{"partial", "shallow", "full"} {
		kind := kind
		s.runCase(kind+"_update", func() map[string]any { return s.update("update-"+kind, kind, "") })
	}
	s.runCase("concurrent_mixed_clones", func() map[string]any {
		return parallel(s.args.concurrency, func(index int) map[string]any {
			kind := []string{"full", "partial", "shallow"}[index%3]
			repo, details := s.clone(fmt.Sprintf("concurrent-clone-%d", index), kind, "")
			if kind == "partial" {
				details["lazy_blob"] = s.lazyBlob(repo, nil)
			}
			if kind == "full" {
				statuses := details["cache_statuses"].([]string)
				check(len(statuses) > 0 && statuses[len(statuses)-1] == "HIT", "concurrent ordinary clone missed cache")
			}
			return details
		})
	})
	s.runCase("concurrent_mixed_updates", func() map[string]any {
		repos := make([]string, s.args.concurrency)
		for i := range repos {
			repos[i] = s.prepareUpdate(fmt.Sprintf("concurrent-update-%d", i), []string{"full", "partial", "shallow"}[i%3])
		}
		return parallel(s.args.concurrency, func(index int) map[string]any {
			return s.update(fmt.Sprintf("concurrent-update-%d", index), []string{"full", "partial", "shallow"}[index%3], repos[index])
		})
	})
	s.runCase("concurrent_cold_cache", s.coldCache)
	s.runCase("cache_update_replay_real_history", s.replayUpdate)
	s.runCase("health", func() map[string]any {
		var health struct {
			Status string `json:"status"`
		}
		must(json.Unmarshal(s.get(s.args.base, "/healthz"), &health))
		check(health.Status == "ok", "server failed health check")
		return nil
	})
	passed := true
	for _, item := range s.cases {
		if item["passed"] != true {
			passed = false
		}
	}
	s.report["metrics_after"], s.report["finished_at"], s.report["passed"] = s.metrics(s.args.base), time.Now().UTC().Format(time.RFC3339), passed
	s.save()
	fmt.Println("Report:", filepath.Join(s.args.output, "results.json"))
	return passed
}

func parseOptions() options {
	var args options
	flag.StringVar(&args.base, "cdn-base", "http://127.0.0.1:8080", "Running multi-site CDN URL")
	flag.StringVar(&args.repository, "repository", "", "Public GitHub owner/repository, for example git/git (required)")
	flag.StringVar(&args.origin, "origin", "https://github.com", "Origin for isolated cold-cache validation")
	flag.StringVar(&args.binary, "binary", "bin/git-cdn", "Git CDN executable for temporary services")
	flag.StringVar(&args.output, "output", "", "New artifact directory (default: bin/live-tests/<owner>-<repository>-<timestamp>)")
	flag.IntVar(&args.concurrency, "concurrency", 6, "Concurrent clients")
	flag.IntVar(&args.timeout, "timeout", 180, "Deadline for each Git command, in seconds")
	flag.Parse()
	args.repository = strings.TrimSuffix(args.repository, ".git")
	valid := regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(args.repository)
	for _, part := range strings.Split(args.repository, "/") {
		if part == "." || part == ".." {
			valid = false
		}
	}
	if !valid || flag.NArg() != 0 || args.concurrency < 1 || args.timeout < 1 {
		fmt.Fprintln(os.Stderr, "require -repository owner/repository, positive concurrency/timeout and no positional arguments")
		flag.Usage()
		os.Exit(2)
	}
	if args.output == "" {
		args.output = filepath.Join("bin", "live-tests", strings.ReplaceAll(args.repository, "/", "-")+"-"+time.Now().Format("20060102-150405.000000"))
	}
	var err error
	args.output, err = filepath.Abs(args.output)
	must(err)
	args.binary, err = filepath.Abs(args.binary)
	must(err)
	return args
}

func main() {
	args := parseOptions()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	s := &suite{args: args, ctx: ctx, url: strings.TrimRight(args.base, "/") + "/github/" + args.repository,
		client: &http.Client{Timeout: 10 * time.Second}, report: map[string]any{
			"started_at": time.Now().UTC().Format(time.RFC3339), "origin": strings.TrimRight(args.origin, "/") + "/" + args.repository,
			"cdn_url": strings.TrimRight(args.base, "/") + "/github/" + args.repository, "concurrency": args.concurrency,
			"update_method": "fetch an older real commit, then fetch the current branch; no upstream writes",
		}}
	_, err := os.Stat(args.output)
	if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "output must be a new directory:", args.output)
		os.Exit(2)
	}
	must(os.MkdirAll(filepath.Dir(args.output), 0700))
	must(os.Mkdir(args.output, 0700))
	must(os.Mkdir(filepath.Join(args.output, "logs"), 0700))
	if binary, err := os.Executable(); err == nil {
		if data, err := os.ReadFile(binary); err == nil {
			s.report["tool_sha256"] = fmt.Sprintf("%x", sha256.Sum256(data))
		}
	}
	defer func() {
		if err := recover(); err != nil {
			s.report["passed"], s.report["error"] = false, fmt.Sprint(err)
			s.save()
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}()
	passed := s.run()
	if !passed {
		s.report["passed"] = false
		s.save()
		os.Exit(1)
	}
}
