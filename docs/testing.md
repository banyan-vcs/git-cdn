# Testing

## Continuous integration

[GitHub Actions](../.github/workflows/ci.yml) runs for pull requests, pushes to `main` and `next`, and manual dispatches. It ensures Git can create reftable repositories and installs the Git LFS client for SSH/LFS checkout and upload tests. It builds, vets, runs the Go suite with race detection and executes the native Git protocol probes on Linux and macOS using the version in `go.mod`. Linux also checks the latest stable Go release. The Linux job using `go.mod` checks formatting, verifies that module files are tidy and fuzzes the protocol parser for ten seconds.

The suite includes the six-client offline live-harness matrix. Test origins run on loopback; public-repository live tests remain an explicit separate operation. CI downloads tools and Go modules, but Git test scenarios require no external repositories or credentials. Module/build caches are reused, and a newer run cancels an older run for the same pull request or branch.

## Local correctness

Run the deterministic Go suite first:

```sh
go test ./...
go test -race ./...
go vet ./...
```

Integration tests run a local authenticated native `git http-backend`. They cover clone/fetch/push, v2 request classification, both site modes and URL suffixes, live authorization on cache hits, isolation, deleted refs, missing objects, LRU, capacity admission, staging cancellation, gzip, redirects and GitHub missing-owner responses.

Cache lifecycle tests additionally cover cross-credential reuse with independent authorization, migration of legacy credential directories without refetch, geometric pack reduction with root connectivity, budget failure preserving packs, queued leases delaying maintenance, full-clone ref pruning and single-branch clones leaving unrequested objects uncached. Roots reach `git fetch` on standard input: one hydration of 150 roots completes and is reused. A hydration that cannot publish is attempted once and deferred until its backoff passes, while a caller that already left is not counted as a failure.

Coverage tests drive standard Git clients against repositories that used to miss the cache. A default `git clone` of a repository with a hundred branches — more wants than the old cap — hydrates once and serves the second clone without upstream traffic. A repository with 134 tags, annotated ones behind the wanted tip included, keeps an ordinary `git fetch` on the local path. A developer's unpushed commits, which Git sends as haves, are dropped from the request while the fetch itself stays local and correct. At the cache level, the batched ancestry decision names the covered candidates of a mixed line-up behind the same tag-object, missing-object and duplicate gate as the all-or-nothing check, and bisects a batch whose escaping history overflows the metadata output limit.

Failure-backoff tests pin the cost of work that cannot succeed. A scope whose hydration fails is not re-hydrated for the next requests: three requests cost one failed attempt and two deferrals. A response above `max_response_bytes` is generated once per backoff period instead of twice per request. A collection whose deadline it cannot meet is retried under a doubling backoff — a handful of attempts where every scan used to restart it — and the commit-graph it rebuilds stays valid next to later history and is replaced, not rejected, by the next collection.

Concurrency tests cover the reader/writer repository gate (shared readers running in parallel, waiting writers blocking fresh readers, context cancellation unpinning waiters, queued requests preempting maintenance — a request that already gave up does neither), warm incremental fetches serving concurrently under shared leases, response-pack single-flight across concurrent identical clones, streaming pins protecting in-flight responses from eviction, shutdown joining an in-flight background reconciliation, reference writes in reftable and files repositories discarding the lock of a writer killed mid-transaction (hydration, reconciliation and grace-ref expiry), and orphaned response-pack staging files being swept at startup. Eviction moves a scope's directory aside under the store mutex and deletes its contents in the background; the managed tree is consistent before the deletion starts.

Authorization-probe tests cover concurrent probes with the same repository and credential merging into one upstream request, the opt-in lifetime reuse including expiry and the revocation window, denials never being reused, credential and repository key separation, upstream failures relaying to merged waiters without wedging the flight, and the shared probe outliving a disconnected leader. Lifetime entries leave the cache when their lifetime ends without a lookup. An advertisement the parser cannot read, or one above `max_refs_bytes`, passes the fetch through to the origin instead of answering 502, and cached objects are never served on its account.

Partial-clone tests cover shallow and filtered fetches serving locally with verbatim reuse, batch `--depth=1` fleets collapsing to one generation under the shared-lease flight, real `git clone --depth=1` and `--filter=blob:none` round-trips including lazy blob reads, and shallow traffic reconciling storage refs through the debounced background prune — snapshot drift retires old roots into the retention grace period end to end, also when a killed reference writer left its lock in the scope, unchanged fingerprints never re-reconcile, accumulated hydrations force one, and a busy scope defers the run until it goes idle. Response-broadcast tests cover concurrent identical hits sharing one stored read byte for byte, slow consumers detaching to their own file reads without divergence, and a low-memory stress workload serving a concurrent mixed batch under a tight soft memory limit.

Hot-path tests pin the concurrency envelope: concurrent warm requests stay in the latency ballpark of sequential ones instead of serializing, warm hits allocate less than the response size per request (block buffers come from the registry pool, whose slots return once streams end), the response-pack index survives restarts, self-heals truncated or stale entries, and recency stamps throttle with a shutdown flush that preserves last-touch ordering. The response-pack key ignores the client's agent line, entries are pruned least recently served first, and an entry stored on the shared reader path is charged at once — pruning, a writer's rescan and an external removal each settle its bytes exactly once. Parser tests pin the snapshot fingerprint against ref-order, unborn and missing-ref changes, that a command beyond the packet budget is forwarded rather than rejected and that selection size has no smaller bound, and cache tests pin the pinned-root completeness index: covered wants skip the walk, uncovered or unindexed ones fall back to it.

Admission tests cover the bounded queue: a burst the rate can absorb within the wait is served, a burst beyond one waiting room is still rejected, and a canceled waiter leaves without a slot. An upstream response cut after its headers is reported as an interrupted transfer naming the side that failed, with `panicked=false`. A request whose body stalls gives its slot back once the idle read deadline passes. Egress follows configuration only: a configured proxy and private CA reach both connection pools, the pools are separate, proxy variables from the environment reach neither, and a loopback origin is never sent through the proxy. The broadcast block pool follows the configured stream limit.

GC tests use controlled object mtimes and collection cutoffs without sleeping for the retention window. They verify expiry beyond 14 days, recent cruft surviving geometric repack, retained history at day 13 and expiry after day 14, queued leases preventing collection below the pack threshold, and budget failures retaining objects.

The Go live harness also has an offline integration test. It exercises all clone, update, mixed concurrency, cold-cache and replay scenarios using a temporary local origin and six clients:

```sh
go test ./scripts/live-repository -run TestLiveSuiteOffline
```

The [local protocol probes](protocol-probes.md) require Go and Git but no network:

```sh
go run ./scripts/protocol-probe
go test ./internal/protocol -fuzz=FuzzParseFetch -fuzztime=10s -parallel=4
```

## Optional real repository validation

With a multi-site server running and `bin/git-cdn` built, select a public workload explicitly:

```sh
go run ./scripts/live-repository --repository git/git --concurrency 6
go run ./scripts/live-repository --repository redis/redis --concurrency 6
```

`git/git` is the primary reference workload. `redis/redis` is a recognizable practical server repository. Large projects such as `kubernetes/kubernetes` or `torvalds/linux` are appropriate only when disk, timeouts and transfer budgets are prepared for them; the full matrix retains multiple complete and partial client repositories.

The isolated cold-cache server uses GitHub by default. Override `-origin` when validating a CDN configured for a local replay origin; use the same origin as the running CDN.

The script checks blob:none and lazy reads, checkout, depth/deepen/unshallow, historical-to-current updates, URL alias reuse, mixed concurrent requests and cold-cache hydration sharing. It also replays a ref update on a local HTTP origin backed by real project objects. That replay is a cache correctness test, not a write to the public upstream.

Output defaults to `bin/live-tests/<owner>-<repository>-<timestamp>/`. It contains command timings, protocol/curl traces, metrics, client repositories and test caches. Keep these artifacts out of version control. The script does not alter the main service or its cache configuration and closes its temporary services. It reports failures if the current configuration or resource budget cannot support its requested local path.

## Performance measurement

The live suite is a correctness harness. Its one-shot elapsed times are useful observations, not a latency or throughput baseline. It does not currently measure every internal phase or TTFB.

For a performance study:

1. Pin the upstream commit, client/server Git versions, binary revision, CPU/memory, disk and network environment.
2. Separate ordinary, partial and shallow traffic. Record final fetch HIT/MISS/BYPASS; discovery passthrough is not a pack miss.
3. Compare direct-origin traffic, an isolated empty CDN cache and a warmed CDN cache using equivalent commands and fresh client directories.
4. Repeat each workload, sweep concurrency and report distributions, failures, queueing and cache metrics. Include transfer bytes, CPU and disk I/O. Add phase and TTFB instrumentation before attributing cost to packing or networking.
5. Record whether update tests use an actual upstream change or controlled local history replay. Incremental pack size is an observation and need not be smaller for every change.

Put run data in ignored artifacts or an external benchmark report. Durable documentation should retain the methodology and acceptance criteria, not dated service IDs, PIDs, machine paths, fixed commit hashes or a single project's timings.

SSH integration tests use a local password-authenticated SSH Git origin and standard Git/OpenSSH clients. They cover ordinary/warm clones, origin updates and subsequent cache hydration, shallow clones serving locally, access revocation and rejected paths. Unit tests cover the silence-bounded session clock (progress moves the deadline, local work is not silence, silence cancels with its cause), concurrent ref lookups sharing one origin connection, the authentication throttle, draining with a grace period, and reaching the origin through HTTP CONNECT and SOCKS5 proxies with and without credentials. `GIT_SSH_COMMAND` carries its paths with forward slashes — a shell parses the variable — so the client tests also run on Windows; there the symlink, file-lock and accept-new host-key tests remain unsupported. See [SSH requirements](ssh.md).
