# Cache policy

## Persistent capacity

`max_cache_bytes` defaults to 20 GiB. It accounts for regular-file lengths in managed `repos/<identity>/<credential>/` scopes, including packs, indexes, refs and repository metadata. One cache directory shares that budget across sites and credentials.

Each scope has a persisted `.access` timestamp. Startup reconstructs size and recency, then trims old scopes if the configured budget is smaller than existing usage. Before publication, capacity is reserved and the oldest idle scopes are evicted. Active and queued leases pin their entry, including while staging reads its objects through alternates.

LRU removes whole scopes, not individual packs or branches. An evicted scope hydrates again on demand. A scope larger than the budget or capacity pinned by active jobs uses upstream passthrough. Old `refs/cache/roots/` are not expired individually; object-level Git GC is not implemented.

The repository lease can end before a spool response finishes transmission because the response file is independent of cached objects. This permits client transfers to overlap without allowing eviction during pack generation.

## Temporary storage

The persistent budget excludes directory/block overhead, store infrastructure and `tmp/`. Each staging repository is checked against the same limit before publication and every 200 ms while fetching. Writes can temporarily exceed that check between samples. Response files have a separate per-response limit through `max_response_bytes`.

Concurrent staging and response files need additional space. These limits are admission controls, not a filesystem quota on the entire cache directory. Track disk usage in addition to exported cache metrics.

## Metrics and verification

Metrics expose cache bytes, capacity, pending reservation bytes, managed entries, eviction count and evicted bytes. Local tests cover LRU order, restart recovery, a smaller startup budget, active/waiting leases, concurrent admission, oversized fallback, growing staging cancellation and refetch after eviction.

See [Testing](testing.md) for the repeatable validation commands. Generated metrics, traces, run-specific service IDs and timings belong in ignored artifacts.

## GitHub missing-repository responses

GitHub's Git endpoint can challenge a missing or inaccessible repository with 401. After that response, the CDN checks the public owner endpoint without sending caller credentials. A valid JSON `Not Found` response for the owner changes the Git response to 404 and removes the login challenge.

Existing owners, metadata failures/rate limits and unknown visibility preserve 401. A repository API 404 alone is insufficient because it may conceal a private repository. This is an owner-based exception for github.com, not a universal method to distinguish nonexistent repositories from private repositories under an existing owner. Other Git hosts keep their upstream status.
