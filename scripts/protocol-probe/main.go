// Run local Git wire-protocol probes without network access or credentials.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chiyutianyi/git-cdn/scripts/internal/testutil"
)

type result struct {
	out, stderr []byte
	code        int
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

func git(dir string, input []byte, check bool, env []string, args ...string) result {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := testutil.Command(ctx, "git", args...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, append(testutil.Environment(), env...), bytes.NewReader(input)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		code = -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		if check || ctx.Err() != nil {
			panic(fmt.Errorf("git %v: %w: %s", args, err, stderr.String()))
		}
	}
	return result{out.Bytes(), stderr.Bytes(), code}
}

func runGit(dir string, args ...string) []byte { return git(dir, nil, true, nil, args...).out }
func oid(dir, ref string) string               { return strings.TrimSpace(string(runGit(dir, "rev-parse", ref))) }
func commit(dir, name, content, message string) string {
	must(os.WriteFile(filepath.Join(dir, name), []byte(content), 0600))
	runGit(dir, "add", name)
	runGit(dir, "commit", "-q", "-m", message)
	return oid(dir, "HEAD")
}

func packet(line string) []byte {
	return []byte(fmt.Sprintf("%04x%s\n", len(line)+5, line))
}

func request(name string, args ...string) []byte {
	data := append(packet("command="+name), []byte("0001")...)
	for _, arg := range args {
		data = append(data, packet(arg)...)
	}
	return append(data, []byte("0000")...)
}

func rpc(dir string, data []byte) result {
	return git("", data, false, []string{"GIT_PROTOCOL=version=2", "GIT_NO_LAZY_FETCH=1"},
		"-c", "uploadpack.allowAnySHA1InWant=false", "-c", "uploadpack.allowTipSHA1InWant=false",
		"-c", "uploadpack.allowReachableSHA1InWant=false", "upload-pack", "--stateless-rpc", dir)
}

func packBytes(data []byte) []byte {
	var pack []byte
	inPack := false
	for len(data) > 0 {
		check(len(data) >= 4, "truncated packet header")
		n, err := strconv.ParseUint(string(data[:4]), 16, 16)
		must(err)
		if n <= 2 {
			data = data[4:]
			continue
		}
		check(n >= 4 && n <= 65520 && int(n) <= len(data), "invalid packet length")
		payload := data[4:n]
		if bytes.Equal(payload, []byte("packfile\n")) {
			inPack = true
		} else if inPack && len(payload) > 0 && payload[0] == 1 {
			pack = append(pack, payload[1:]...)
		}
		data = data[n:]
	}
	return pack
}

func probes() {
	root, err := os.MkdirTemp("", "git-cdn-probe-")
	must(err)
	defer os.RemoveAll(root)
	work, origin, cache := filepath.Join(root, "work"), filepath.Join(root, "origin.git"), filepath.Join(root, "cache.git")
	var checks []map[string]any
	record := func(name string, details map[string]any) {
		details["name"], details["passed"] = name, true
		checks = append(checks, details)
	}
	runGit("", "init", "-q", "--initial-branch=main", "--object-format=sha1", work)
	visible := commit(work, "public.txt", "public data\n", "public commit")
	runGit(work, "checkout", "-q", "--orphan", "secret")
	runGit(work, "rm", "-q", "-rf", ".")
	secret := commit(work, "secret.txt", "private data\n", "private commit")
	runGit(work, "checkout", "-q", "main")
	runGit("", "clone", "-q", "--bare", "--no-hardlinks", work, origin)
	runGit("", "clone", "-q", "--bare", "--no-hardlinks", origin, cache)
	runGit(origin, "update-ref", "-d", "refs/heads/secret")
	refs := rpc(origin, request("ls-refs", "symrefs", "peel"))
	check(refs.code == 0 && bytes.Contains(refs.out, []byte(visible)) && !bytes.Contains(refs.out, []byte(secret)), "unexpected upstream refs")
	response := rpc(cache, request("fetch", "want "+secret, "done"))
	pack := packBytes(response.out)
	check(response.code == 0 && bytes.HasPrefix(pack, []byte("PACK")), "retained OID fetch failed")
	receiver := filepath.Join(root, "receiver.git")
	runGit("", "init", "-q", "--bare", "--object-format=sha1", receiver)
	git(receiver, pack, true, nil, "index-pack", "--stdin")
	check(string(runGit(receiver, "cat-file", "-p", secret+":secret.txt")) == "private data\n", "incorrect retained blob")
	record("v2_fetch_accepts_unadvertised_cached_oid_even_with_allow_flags_false", map[string]any{"upstream_advertised_secret": false, "cache_served_secret": true})

	broken := filepath.Join(root, "broken.git")
	// A local clone keeps loose objects without linking them to the healthy cache.
	runGit("", "clone", "-q", "--bare", "--no-hardlinks", cache, broken)
	blob := oid(broken, visible+":public.txt")
	must(os.Remove(filepath.Join(broken, "objects", blob[:2], blob[2:])))
	tipType := strings.Fields(string(git(broken, []byte(visible+"\n"), true, nil, "cat-file", "--batch-check").out))[1]
	missing := runGit(broken, "rev-list", "--objects", "--missing=print", visible)
	check(tipType == "commit" && bytes.Contains(missing, []byte("?"+blob)), "missing closure was not detected")
	failed := rpc(broken, request("fetch", "want "+visible, "done"))
	check(failed.code != 0 && bytes.Contains(failed.out, []byte("packfile\n")), "expected failure after output began")
	record("existing_want_does_not_prove_dependency_closure", map[string]any{"want_type": tipType, "missing_objects": 1, "upload_pack_exit": failed.code, "output_started_before_failure": true})

	newTip := commit(work, "new.txt", "new data\n", "new commit")
	runGit(work, "push", "-q", origin, "main")
	check(git(cache, nil, false, nil, "cat-file", "-e", newTip).code != 0, "new tip already cached")
	runGit(cache, "-c", "protocol.version=2", "fetch", "--no-tags", "--no-recurse-submodules", "--no-auto-maintenance", "--no-write-fetch-head", "file://"+filepath.ToSlash(origin), newTip+":refs/cache/roots/"+newTip)
	closure := runGit(cache, "rev-list", "--objects", "--missing=print", newTip)
	check(!bytes.Contains(append([]byte("\n"), closure...), []byte("\n?")), "hydrated closure has holes")
	fetched := rpc(cache, request("fetch", "want "+newTip, "done"))
	check(fetched.code == 0 && bytes.HasPrefix(packBytes(fetched.out), []byte("PACK")), "hydrated upload-pack failed")
	record("fetch_by_exact_oid_hydrates_then_local_upload_pack_succeeds", map[string]any{"before_fetch_missing": true, "after_fetch_complete": true})

	shallow := rpc(cache, request("fetch", "want "+newTip, "deepen 1", "done"))
	check(shallow.code == 0 && bytes.Contains(shallow.out, []byte("shallow-info\n")) && bytes.Contains(shallow.out, []byte("shallow "+newTip)), "shallow info missing")
	check(bytes.HasPrefix(packBytes(shallow.out), []byte("PACK")), "shallow pack missing")
	_, err = os.Stat(filepath.Join(cache, "shallow"))
	check(os.IsNotExist(err), "server cache became shallow")
	record("complete_cache_can_serve_shallow_client", map[string]any{"shallow_info_present": true, "server_cache_is_shallow": false})

	negotiation := rpc(cache, request("fetch", "want "+newTip, "have "+visible, "wait-for-done"))
	check(negotiation.code == 0 && bytes.Contains(negotiation.out, []byte("acknowledgments\n")) && !bytes.Contains(negotiation.out, []byte("packfile\n")), "unexpected negotiation-only response")
	record("v2_negotiation_can_return_without_a_pack", map[string]any{"response_has_acknowledgments": true, "response_has_pack": false})
	caps := git("", nil, true, []string{"GIT_PROTOCOL=version=2"}, "upload-pack", "--advertise-refs", origin)
	check(bytes.Contains(caps.out, []byte("version 2\n")) && !bytes.Contains(caps.out, []byte(visible)), "initial advertisement included refs")
	record("v2_initial_advertisement_contains_capabilities_not_refs", map[string]any{"advertisement_has_tip": false})
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	must(encoder.Encode(map[string]any{"git_version": strings.TrimSpace(string(runGit("", "--version"))), "checks": checks}))
}

func main() {
	defer func() {
		if err := recover(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}()
	probes()
}
