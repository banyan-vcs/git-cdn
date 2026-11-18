# Cache capacity and repository status checks

## Persistent cache policy

`max_cache_bytes` defaults to 20 GiB. The admission budget covers regular-file lengths in managed `repos/<identity>/<credential>/` scopes. It includes pack/index files, refs and repository metadata, and is shared across upstreams and credential scopes in one cache directory.

Each scope has a persisted `.access` timestamp. Startup reconstructs size and recency, then removes oldest scopes if an existing cache is over the configured limit. Before publishing new data, the store reserves additional capacity and evicts the least recently used idle scopes. Active and queued leases pin an entry, including while staging borrows its objects through alternates. A spool response can finish transmitting after its repository lease has ended because the response file is independent of the object store.

Oversized repositories and capacity pinned by other active jobs use upstream passthrough. A future request for an evicted repository hydrates it again. Growth of each staging repository is checked every 200 ms and before publication, using the same limit. `tmp/`, directory/block overhead and store infrastructure are outside the persistent budget; concurrent staging and response spools can consume additional disk space. The limit is not a filesystem quota.

Metrics expose current cache bytes, maximum bytes, capacity reservations, managed entries, eviction count and evicted bytes. Policy tests cover LRU ordering, restart recovery, lowering the limit, concurrent admission, pinned/queued leases, oversized fallback, staging cancellation and refetch after eviction.

## Missing GitHub repository

The real example `https://github.com/banyan-xxx/banyan` produced these upstream results:

| Endpoint | Status |
| --- | --- |
| Git smart info/refs | 401 with authentication challenge |
| Repository REST API | 404 |
| Public owner API | 404 |
| Repository web page | 404 |

GitHub's Git endpoint intentionally conflates missing and inaccessible repositories. The CDN now checks the public owner endpoint only after a GitHub 401. A valid JSON `Not Found` response for the owner changes the Git response to 404 and removes the challenge. It sends no caller credentials to this public lookup. Existing owners, API errors/rate limits and unknown visibility preserve 401. A repository API 404 alone is insufficient because it can conceal private repositories.

This rule is limited to github.com; other upstreams keep their status. It is not a general mechanism for distinguishing a missing repository from a private repository beneath an existing owner.

Verified against the actual GitHub example through the updated binary:

- Both `/github/banyan-xxx/banyan/info/refs` and the `.git` form return 404 without `WWW-Authenticate`.
- Direct POST to upload-pack also returns 404.
- Standard Git clone exits 128 with `repository ... not found`, without the username prompt error.
- Integration tests retain 401 for an existing owner, rate limits, invalid metadata, service failures and non-GitHub hosts.

