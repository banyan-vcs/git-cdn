# Cache policy

## Persistent capacity

`max_cache_bytes` defaults to 20 GiB. It accounts for regular-file lengths in managed `repos/<identity>/` repositories, including packs, indexes, refs and repository metadata. Authorized users share the same repository objects; one cache directory shares that budget across sites and repositories.

Each scope has a persisted `.access` timestamp. Startup reconstructs size and recency, then trims old scopes if the configured budget is smaller than existing usage. Before publication, capacity is reserved and the oldest idle scopes are evicted. Active and queued leases pin their entry, including while staging reads its objects through alternates.

LRU removes whole repositories. An evicted repository hydrates again on demand. A repository larger than the budget or capacity pinned by active jobs uses upstream passthrough.

Full unfiltered fetches with no haves and `done` reconcile cached refs against the complete live snapshot. Available complete roots are pinned and mapped to `refs/cache/upstream/`; obsolete cached refs and old roots are removed. Unrequested branches are not downloaded. Unpinning removes a retention ref, not an individual pack. Source refs and current requested roots still govern every response.

## Pack maintenance

`repack_pack_threshold` defaults to 32 (minimum 2) and `repack_timeout` defaults to two minutes. A bounded background queue coalesces maintenance, and idle-only acquisition uses the existing repository gate. Active and waiting clients pin their repository. Maintenance also consumes the global native Git process limit.

`git repack --geometric=2 -d` runs in a temporary copy with hardlinked immutable objects and copied metadata. New-file bytes have a capacity reservation and are checked every 200 ms and before publication. The staged pack set must have complete ref dependencies before new packs are installed and old packs are removed. Failed or timed-out generation preserves the existing pack set. Capacity shortages skip maintenance. A no-op or failure is retried on a later repository access rather than continuously requeued.

This consolidates packs; it does not implement cruft expiry or general object-level GC. Superseded histories may still occupy disk after their roots are removed.

The repository lease can end before a spool response finishes transmission because the response file is independent of cached objects. This permits client transfers to overlap without allowing eviction during pack generation.

## Temporary storage

The persistent budget excludes directory/block overhead, store infrastructure and `tmp/`. Each staging repository is checked against the same limit before publication and every 200 ms while fetching. Writes can temporarily exceed that check between samples. Local response files use `max_response_bytes` per file and a global `max_temp_response_bytes` budget (4 GiB by default). The global reservation remains held after Git process completion until file removal; exhausted generation falls back before committing local output. Startup clears orphaned service temporary files under the directory lock.

Concurrent staging and response files need additional space. These limits are admission controls, not a filesystem quota on the entire cache directory. Track disk usage in addition to exported cache metrics. Request admission and transfer deadlines bound active responses; see [HTTP security](http-security.md).

## Metrics and verification

Metrics expose cache bytes, capacity, pending reservation bytes, managed entries, eviction count and evicted bytes. Local tests cover LRU order, restart recovery, a smaller startup budget, active/waiting leases, concurrent admission, oversized fallback, growing staging cancellation and refetch after eviction.

See [Testing](testing.md) for the repeatable validation commands. Generated metrics, traces, run-specific service IDs and timings belong in ignored artifacts.

## GitHub missing-repository responses

GitHub's Git endpoint can challenge a missing or inaccessible repository with 401. After that response, the CDN checks the public owner endpoint without sending caller credentials. A valid JSON `Not Found` response for the owner changes the Git response to 404 and removes the login challenge.

Existing owners, metadata failures/rate limits and unknown visibility preserve 401. A repository API 404 alone is insufficient because it may conceal a private repository. This is an owner-based exception for github.com, not a universal method to distinguish nonexistent repositories from private repositories under an existing owner. Other Git hosts keep their upstream status.
