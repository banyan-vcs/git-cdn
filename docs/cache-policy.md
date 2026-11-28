# Cache policy

## Persistent capacity

`max_cache_bytes` defaults to 20 GiB. It accounts for regular-file lengths in managed `repos/<identity>/` repositories, including packs, indexes, refs and repository metadata. Authorized users share the same repository objects; one cache directory shares that budget across sites and repositories.

Each scope has a persisted `.access` timestamp and a `.site` record naming the upstream it was last served for, which only the repository metrics read. Startup reconstructs size and recency, then trims old scopes if the configured budget is smaller than existing usage. Before publication, capacity is reserved and the oldest idle scopes are evicted. Recency writes are throttled to one persisted stamp per scope per 30 seconds — in-memory recency always advances, a suppressed flush marks the scope dirty, and shutdown persists dirty stamps so a clean restart keeps the true last-touch ordering; after a crash the ordering is only as fresh as the last stamp. Active and queued leases pin their entry, including while staging reads its objects through alternates. Validation and pack generation hold shared reader leases: warm serving of one repository runs in parallel, hydration and maintenance hold the writer side, and waiting writers block fresh readers. Streams served from the response pack cache pin their scope without any lease and release the pin — refreshing recency — when the transfer ends, so an in-flight response cannot lose its scope mid-stream. A candidate whose directory cannot be moved aside — a response still streaming on Windows holds its files — is skipped and reported without aborting admission for the remaining candidates. Eviction moves the directory under the store mutex and deletes its contents in the background, so no request waits for another repository's files to disappear.

LRU removes whole repositories. An evicted repository hydrates again on demand. A repository larger than the budget or capacity pinned by active jobs uses upstream passthrough, and does not repeat the attempt for every request: a hydration that fails — out of time, out of capacity, refused — backs the repository off for thirty seconds, doubling up to an hour, and roots that are missing are not fetched again until that passes.

Every locally served fetch, a response-pack hit included, reconciles cached refs against its complete live snapshot in the background — debounced by fingerprint drift or hydration accumulation, and running only while the scope is idle. One run waits per scope and applies the newest snapshot asked for by the time it takes the scope; a run that finds the scope busy for two seconds gives up, and the next fetch served arms it again. A request arms it before it releases the scope, so a run a request preempted applies that request's snapshot, never the older one it was started for. Available complete roots are pinned and mapped to `refs/cache/upstream/`; roots the snapshot no longer names retire into the retention grace period, which bounds root accumulation for fleets that never stop at any advertised ref. Unrequested branches are not downloaded. Unpinning removes a retention ref, not an individual pack. Source refs and current requested roots still govern every response.

## Ref storage

New object caches, hydration staging and client ref views use `git init --bare --ref-format=reftable`. Legacy credential-cache merging also creates a reftable target when no shared repository exists. Native Git 2.45+ is required, and startup probes this capability before opening the cache. Ref lookup and updates benefit from indexed storage and fewer loose files when repositories have many branches or tags. Upstream discovery and `ls-refs` continue to use live origin authorization.

HEAD is read and written through native Git, since the on-disk `HEAD` in a reftable repository is a compatibility marker. Ref update admission reserves room for new records, table-stack compaction and backend metadata; existing files repositories retain their packed-refs rewrite allowance. Repack and GC copy the repository's current backend into private staging and preserve its format.

Git deletes its lock and temporary files when a reference transaction ends, and again through its signal handlers when it is terminated. A Git process the service cancels is therefore terminated first on Linux and macOS, and killed when it has not exited two seconds later; Windows ends it at once. One that dies without that chance — killed, or lost with the service — leaves the files behind: in a reftable repository the stack lock, the temporary file it was writing and any table the table list does not name; in a files repository the locks on `HEAD`, `packed-refs` and the references it was moving. The lock makes every later transaction in the repository fail with `cannot lock`. Reference transactions only run where the previous writers are known to have exited: under the writer side of the repository gate, the store's directory lock during migration, or in a maintenance staging copy. Before its own, the writer discards what a killed one left there — a reftable repository by its table list, a files repository for the references the transaction writes — so the repository keeps hydrating and reconciling instead of waiting for the files to be removed by hand.

Existing files-format caches stay usable without downloading their objects again. To convert one, stop all users of its cache directory, then use Git 2.46+:

```sh
git -C /path/to/cache/repos/REPOSITORY_HASH/repo.git refs migrate --ref-format=reftable
```

The service's newly created temporary repositories use reftable even when serving an existing files cache. Migration is optional; the service does not run it on existing shared caches automatically.

## Pack maintenance

`repack_pack_threshold` defaults to 32 (minimum 2) and `repack_timeout` defaults to two minutes. A bounded background queue coalesces maintenance, and idle-only acquisition uses the existing repository gate. Active and waiting clients pin their repository. Maintenance also consumes the global native Git process limit.

`git repack --geometric=2 -d` runs in a temporary copy with hardlinked immutable objects and copied metadata. New-file bytes have a capacity reservation and are checked every 200 ms and before publication. The staged pack set must have complete ref dependencies before new packs are installed and old packs are removed. Failed or timed-out generation preserves the existing pack set. Capacity shortages skip maintenance. A failure is retried on a later repository access or scan once its backoff has passed: one minute after the first failure, doubling up to an hour. An attempt that a queued request cancels is neither a failure nor a success — it keeps its place in the schedule — so a busy repository does not accumulate backoff it never earned. The queue decides from the pack count recorded by the last writer; it does not list the directory while holding the store mutex.

## Unreachable-object retention

`gc_interval` defaults to 24 hours. A minute-based maintenance scan also queues idle repositories whose persisted `.gc-last` is due; activity and queued leases prevent acquisition. GC does not depend on pack count and has its own deadline, `gc_timeout` (30 minutes by default), because a cruft repack rewrites every pack where a geometric repack touches a few; it shares the Git process limit. Successful runs advance `.gc-last`. Failed and capacity-skipped attempts retry on later activity or a scan under the same backoff as repack failures, instead of being restarted by every scan until they time out again.

GC also rebuilds the commit-graph, the one pass that already touches every commit: `git commit-graph write --reachable` runs in staging and the file is installed by rename with the packs. Ancestry checks — client haves, include-tag targets and the pinned-root index — then stop at generation boundaries instead of walking whole histories. The graph's growth over the graph it replaces needs its own capacity reservation; when it does not fit, the repository keeps its previous graph or none, and stays correct either way.

`gc_retention` defaults to 336 hours and cannot be shorter than 14 days. Cruft repack (`-a -d --cruft --cruft-expiration=<cutoff>`) and loose pruning run in a private repository. Recent unreachable objects and dependencies are retained using object mtimes and `.mtimes`; these are storage ages, not commit author/committer dates. Geometric repack excludes cruft packs, and publication includes their `.mtimes` sidecars.

Ref reconciliation moves removed roots to `refs/cache/retained/<oid>` and persists their removal time in `.gc-retained`. A renewed live root clears its old grace period; a later removal starts a new period. Whole histories receive at least 14 days from unpinning, even when their object storage is older. Missing ref timestamp metadata renews the grace period conservatively. Internal grace refs never authorize reads or appear in client ref views.

GC expires grace pins in staging, validates the surviving object closure and publishes pack data and age metadata before deleting expired pins and superseded objects. Budget, timeout or validation failure during generation leaves the published repository intact. Whole-repository LRU eviction is an independent recoverable-cache policy, and still cannot remove active or queued repositories.

The repository lease can end before a spool response finishes transmission because the response file is independent of cached objects. This permits client transfers to overlap without allowing eviction during pack generation.

## Response pack cache

Cacheable requests — done-terminated fetches without client haves, which
includes shallow and filtered shapes — reuse their completed responses
verbatim. Entries live under `repos/<identity>/packcache/`, keyed by the
repository identity, the exact decoded request bytes and the live ref
snapshot, so a new tip or tag produces a new key. A hit streams the stored
file without consulting repository objects, hydration or upload-pack; the
per-request upstream authorization probe is unchanged. Entry bytes count
toward the scope size and the persistent budget from the moment they are
stored, whichever lease stored them: the in-memory index is the authority
for those bytes, and a directory rescan adds the indexed sizes instead of
counting the files again, so storing, pruning, invalidating and losing a
file each adjust the budget exactly once. Per-scope retention is bounded by
`max_pack_cache_bytes` (least recently served first; oversized responses
never stored). Recency lives in memory — after a restart the publish order
stands in until an entry is served again. An in-memory index maps each
entry to its published size, so a warm lookup validates one file instead of
scanning the directory; the index is rebuilt at startup, publication removes
stale same-key files, and a mismatched or missing file drops out of the
index and the directory so the next request regenerates. Concurrent
identical requests collapse to one generation per entry — on a warm scope a
shared-lease flight serializes the upload-pack run — and warm hits share
one read of the entry through a bounded in-memory broadcast whose block
buffers come from a pooled free list (drained by the collector under memory
pressure) and whose slow consumers fall back to their own file reads. The
broadcast's block pool follows the configured request and SSH concurrency,
so raising those limits does not leave a fast stream waiting on a slow
reader's slots. Startup also sweeps staging leftovers of interrupted
publications under the directory lock. See
[Configuration](configuration.md#response-pack-cache).

## Temporary storage

The persistent budget excludes directory/block overhead, store infrastructure and `tmp/`. Each staging repository is checked against the same limit before publication and every 200 ms while fetching. Writes can temporarily exceed that check between samples. Local response files use `max_response_bytes` per file and a global `max_temp_response_bytes` budget (4 GiB by default). The global reservation remains held after Git process completion until file removal; exhausted generation falls back before committing local output. Startup clears orphaned service temporary files under the directory lock.

Concurrent staging and response files need additional space. These limits are admission controls, not a filesystem quota on the entire cache directory. Track disk usage in addition to exported cache metrics. Request admission and transfer deadlines bound active responses; see [HTTP security](http-security.md).

## Metrics and verification

Metrics expose cache bytes, capacity, pending reservation bytes, managed entries, eviction count and evicted bytes. Local tests cover LRU order, restart recovery, a smaller startup budget, active/waiting leases, concurrent admission, oversized fallback, growing staging cancellation and refetch after eviction.

See [Testing](testing.md) for the repeatable validation commands. Generated metrics, traces, run-specific service IDs and timings belong in ignored artifacts.

## GitHub missing-repository responses

GitHub's Git endpoint can challenge a missing or inaccessible repository with 401. After that response, the CDN checks the public owner endpoint without sending caller credentials. A valid JSON `Not Found` response for the owner changes the Git response to 404 and removes the login challenge.

Existing owners, metadata failures/rate limits and unknown visibility preserve 401. A repository API 404 alone is insufficient because it may conceal a private repository. This is an owner-based exception for github.com, not a universal method to distinguish nonexistent repositories from private repositories under an existing owner. Other Git hosts keep their upstream status.
