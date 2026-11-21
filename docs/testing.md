# Testing

## Continuous integration

[GitHub Actions](../.github/workflows/ci.yml) runs for pull requests, pushes to `main` and manual dispatches. It ensures Git can create reftable repositories and installs the Git LFS client for SSH/LFS checkout and upload tests. It builds, vets, runs the Go suite with race detection and executes the native Git protocol probes on Linux and macOS using the version in `go.mod`. Linux also checks the latest stable Go release. The Linux job using `go.mod` checks formatting, verifies that module files are tidy and fuzzes the protocol parser for ten seconds.

The suite includes the six-client offline live-harness matrix. Test origins run on loopback; public-repository live tests remain an explicit separate operation. CI downloads tools and Go modules, but Git test scenarios require no external repositories or credentials. Module/build caches are reused, and a newer run cancels an older run for the same pull request or branch.

## Local correctness

Run the deterministic Go suite first:

```sh
go test ./...
go test -race ./...
go vet ./...
```

Integration tests run a local authenticated native `git http-backend`. They cover clone/fetch/push, v2 request classification, both site modes and URL suffixes, live authorization on cache hits, isolation, deleted refs, missing objects, LRU, capacity admission, staging cancellation, gzip, redirects and GitHub missing-owner responses.

Cache lifecycle tests additionally cover cross-credential reuse with independent authorization, migration of legacy credential directories without refetch, geometric pack reduction with root connectivity, budget failure preserving packs, queued leases delaying maintenance, full-clone ref pruning and single-branch clones leaving unrequested objects uncached.

Concurrency tests cover the reader/writer repository gate (shared readers running in parallel, waiting writers blocking fresh readers, context cancellation unpinning waiters, queued requests preempting maintenance), warm incremental fetches serving concurrently under shared leases, response-pack single-flight across concurrent identical clones, streaming pins protecting in-flight responses from eviction, shutdown joining an in-flight background reconciliation, and orphaned response-pack staging files being swept at startup.

Authorization-probe tests cover concurrent probes with the same repository and credential merging into one upstream request, the opt-in lifetime reuse including expiry and the revocation window, denials never being reused, credential and repository key separation, upstream failures relaying to merged waiters without wedging the flight, and the shared probe outliving a disconnected leader.

Partial-clone tests cover shallow and filtered fetches serving locally with verbatim reuse, batch `--depth=1` fleets collapsing to one generation under the shared-lease flight, real `git clone --depth=1` and `--filter=blob:none` round-trips including lazy blob reads, and shallow traffic never running full-fetch ref reconciliation. Response-broadcast tests cover concurrent identical hits sharing one stored read byte for byte, slow consumers detaching to their own file reads without divergence, and a low-memory stress workload serving a concurrent mixed batch under a tight soft memory limit.

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

SSH integration tests use a local password-authenticated SSH Git origin and standard Git/OpenSSH clients. They cover ordinary/warm clones, origin updates and subsequent cache hydration, shallow passthrough, access revocation and rejected paths. See [SSH requirements](ssh.md).
