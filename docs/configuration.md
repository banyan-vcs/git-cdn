# Configuration

All settings live in one YAML file. The default is
`~/.git-cdn/config.yaml`; override it with a command-line argument:

```sh
./bin/git-cdn -config ./config.yaml
```

The configuration path and `cache_dir` expand `~/` to the current user's
home directory. Explicit relative paths resolve against the process's
startup working directory. Changes take effect after a restart. Mixing
`upstream` and `upstreams`, unknown fields, duplicate keys and multiple
YAML documents are rejected at startup.

See [config.example.yaml](../config.example.yaml) (single site),
[config.multi.example.yaml](../config.multi.example.yaml) (multi site) and
[config.ssh.example.yaml](../config.ssh.example.yaml) (with the SSH
frontend) for complete commented examples.

## Modes

Single-site mode maps repository paths directly to one fixed host:

```yaml
mode: single
upstream:
  url: https://gitlab.com
```

```text
https://git-cdn.example/group/repo[.git]
    → https://gitlab.com/group/repo.git
```

Multi-site mode selects the host by the first path segment:

```yaml
mode: multi
upstreams:
  gitlab:
    url: https://gitlab.com
    credential_host: git-cdn-gitlab.example
  github:
    url: https://github.com
    credential_host: git-cdn-github.example
```

```text
https://git-cdn-gitlab.example/gitlab/group/repo[.git]
    → https://gitlab.com/group/repo.git
https://git-cdn-github.example/github/owner/repo[.git]
    → https://github.com/owner/repo.git
```

When `mode` is omitted, `upstream` selects single-site mode and `upstreams`
selects multi-site mode; a one-entry `upstreams` map still uses a site
prefix. Both modes preserve path case and normalize URLs with or without
`.git` to the same upstream path and cache identity. Upstream URLs may
include a base path. Discovery, ls-refs, fetch, push and standard
`info/lfs` routes normalize the suffix before forwarding POST requests.
Redirects retain the direct repository path in single-site mode and the
site prefix in multi-site mode.

## Settings

| Setting | Default | Purpose |
| --- | --- | --- |
| `cache_dir` | `~/.git-cdn/caches` | Persistent objects, staging, response files, and the process lock |
| `max_cache_bytes` | 20 GiB | Total logical file bytes across persistent repository cache scopes; idle scopes are evicted by LRU |
| `repack_pack_threshold` | 32 | Queue idle repositories at or above this pack count; minimum 2 |
| `repack_timeout` | 2m | Deadline for a background geometric repack |
| `gc_interval` | 24h | Interval between successful garbage collections, independent of pack count |
| `gc_timeout` | 30m | Deadline for a background garbage collection; a cruft repack rewrites every pack |
| `gc_retention` | 336h | Minimum 14-day window for recent unreachable objects and removed-root histories |
| `mode` | Inferred from upstream/upstreams | Direct paths for single; site-prefixed paths for multi |
| `upstream` | Required for single | One fixed host with a `url` field |
| `upstreams` | Required for multi | Site ID to upstream mapping |
| `upstream_proxy` | none | Proxy for origin connections: `http`, `https`, `socks5` or `socks5h` URL; see [Outbound proxy and private CA](#outbound-proxy-and-private-ca) |
| `upstream_ca_file` | system roots | PEM bundle that replaces the system roots for HTTPS upstream verification |
| `max_request_bytes` | 8 MiB | Compressed and decompressed v2 upload-pack POST limit |
| `max_refs_bytes` | 16 MiB | Internal ls-refs response limit; a larger advertisement sends the fetch upstream |
| `max_response_bytes` | 1 GiB | Local response spool limit; a larger response passes through and is not attempted again until its backoff passes |
| `max_concurrent_git` | 4 | Native Git process concurrency |
| `max_concurrent_requests` | 16 | Active Git requests, held through parsing, passthrough and response transfer |
| `max_requests_per_second` | 64 | Instance-wide rate, with an initial burst of the same size; requests over it queue for up to one second |
| `max_temp_response_bytes` | 4 GiB | Total bytes in unfinished local responses, released after file deletion |
| `max_pack_cache_bytes` | 1 GiB | Per-repository budget for verbatim response-pack reuse; `0` applies the default, a negative value disables it |
| `write_idle_timeout` | 30s | Deadline for each write/flush and for each read of a request body; progressing transfers refresh it |
| `request_timeout` | 10m | Total Git request deadline over HTTP, including input, origin access and transfer. Over SSH it is the time without traffic or local work |
| `fetch_timeout` | 2m | Deadline for each hydration operation; a hydration that fails or runs out of time backs the repository off before the next attempt |
| `git_timeout` | 5m | Total local deadline, including queueing, validation, hydration, and packing |
| `upstream_timeout` | 30s | Deadline for the live ref probe |
| `auth_probe_cache_ttl` | 0s (disabled) | Lifetime for reusing a successful authorization probe; see [Authorization probes](#authorization-probes) |
| `memory_limit_bytes` | 0 (disabled) | Go soft memory limit for the service process; the collector works harder near the limit instead of growing toward an out-of-memory kill. Bounds this process only — child Git processes and the on-disk budgets are unaffected |

```yaml
max_cache_bytes: 21474836480 # 20 GiB
```

The cache budget is a persistent-cache admission limit, not a filesystem
quota: it counts regular-file lengths under managed `repos/` scopes
(including Git metadata) and excludes directory/block overhead, the process
lock and `tmp/`. Concurrent staging and response files need additional
disk space. Run one service process per cache directory, and do not prune
or delete objects while it runs. Cache eviction, maintenance and retention
details: [cache-policy.md](cache-policy.md).

## Authorization probes

Every local fetch authorizes through a live upstream ls-refs probe carrying
the caller's credentials. Two mechanisms bound probe traffic without
touching the object cache's authorization boundary:

Concurrent probes for the same repository and credential always merge into
one upstream request. Waiters reuse the leader's fresh result, so merging
is semantically transparent and always enabled: a clone storm of identical
requests costs one probe. Merged and cached outcomes are observable through
`gitcdn_auth_probes_merged_total` and `gitcdn_auth_probe_cache_hits_total`
on `/metrics`.

`auth_probe_cache_ttl` additionally reuses a successful probe for a short
lifetime, keyed by repository and a hash of the forwarded credential
headers (in memory only, never persisted). This is an explicit tradeoff:
authorization degrades from live to near-live, and a revoked credential
stays readable for at most the configured lifetime — 30–60s keeps origin
probe rates at roughly one per repository and credential per interval.
Only successful probes are reused; denials always re-probe, and a probe
that has not yet completed still merges concurrent waiters. When enabled,
`X-GitCDN-Auth-Cache: HIT|LIVE` on local responses reports the outcome.
Entries expire on a timer once per lifetime — without waiting to be looked
up again — and are bounded by count (8192) and by the total number of refs
they hold (about one million), since each entry keeps a whole ref snapshot.
The default `0s` keeps authorization fully live. For fleet-scale probe
convergence across CDN nodes, see the edge/shield design discussion in the
project's scaling notes.

## Request limits

Each v2 command is classified for at most 4096 pkt-lines without
materializing a packet array. A longer command is forwarded upstream
unparsed instead of being rejected: a default `git clone` sends one want per
advertised ref, and repositories with more refs than that still clone, only
without the cache. There is no smaller cap on wants, haves or tags — within
the packet budget a fetch for every branch and tag of a large repository is
served locally, and haves or tags that the cache cannot place inside the
authorized history are dropped from the request rather than sent upstream.
Gzip v2 requests are decoded for inspection and forwarded with their
original compressed body; other encodings forward as-is. Canonical
redirects within the trusted upstream base path are rewritten to the CDN.
Git requests over the concurrency or rate limits queue for up to one
second — Git does not retry a 503, so a short burst fails no clone — and
return 503 with `Retry-After: 1` when capacity has not freed up by then.
The queue holds one burst of waiting requests; requests beyond it are
rejected at once. Health and metrics requests do not consume Git request
slots. A client that stops sending its request body is disconnected after
`write_idle_timeout`; there is no absolute limit on how long an upload may
take while it keeps making progress.

## Response pack cache

Clone-shaped requests — v2 fetches with wants, `done` and no haves, the
shape CI clone storms produce — additionally reuse the generated response
verbatim. The entry key binds the repository identity, the decoded request
bytes and the live ref snapshot: a new tip, a new tag or different
capabilities produce a different key and a fresh generation. The `agent`
line is left out of the key, because the client's Git version does not
change the response; clients on different versions or operating systems
share one entry, and a fleet-wide Git upgrade no longer empties the cache.
Requests with client haves never enter the cache. Reuse skips hydration,
ref views and upload-pack entirely; the live upstream authorization probe
still runs for every request, and hits are served without taking the
repository lock, so concurrent warm clones of one repository stream in
parallel. Entries are self-contained files under the repository scope
(`repos/<hash>/packcache/`): they count toward the persistent cache budget
as soon as they are stored, survive restarts, and disappear with
whole-scope LRU eviction. Within a scope, `max_pack_cache_bytes` (1 GiB by
default, per repository) evicts the least recently served entries first —
in memory, so a long-lived hot entry outlives a freshly generated cold
one; after a restart the publish order stands in until entries are served
again. Responses larger than the budget are never retained.
`X-GitCDN-Pack-Cache: HIT|MISS` on local responses reports the outcome, and
`gitcdn_pack_cache_hits_total` / `gitcdn_pack_cache_stores_total` are
exported on `/metrics`.

A response that cannot be produced — larger than `max_response_bytes`, or
slower than `git_timeout` — is not generated again for thirty seconds,
doubling up to an hour for as long as it keeps failing. Requests for it
pass through meanwhile instead of each repeating the failed packing.

## Transport and TLS

The listener defaults to `127.0.0.1:8080`. Run behind a TLS reverse proxy
for the CDN domain, or configure both `tls_cert` and `tls_key` to serve
HTTPS directly. A front proxy must forward `Git-Protocol` and the configured
authentication headers, and allow Git request and response bodies.

Non-loopback HTTP listeners require TLS or `trusted_proxy_cidrs`; a
trusted proxy must overwrite `X-Forwarded-Proto: https`, and untrusted
peers cannot bypass the check by supplying this header. Non-loopback
upstreams require HTTPS. Use `allow_insecure_http: true` only on a
controlled internal network. Full transport requirements and migration
notes: [http-security.md](http-security.md).

## Outbound proxy and private CA

Connections to origins ignore proxy variables from the environment
(`http_proxy`, `https_proxy`, `all_proxy`, `no_proxy`); egress follows the
configuration only. `upstream_proxy` routes every origin connection through
one proxy — HTTP and HTTPS upstreams, the Git child processes that hydrate
objects, and SSH upstreams alike:

```yaml
upstream_proxy: http://proxy.example:3128
# Credentials may be embedded: socks5://gitcdn:secret@proxy.example:1080
upstream_ca_file: /etc/git-cdn/origin-ca.pem
```

HTTP proxies are crossed with `CONNECT` and SOCKS5 proxies with the
standard handshake, with or without username/password authentication. A
loopback origin is never sent through the proxy, as with proxy variables
from the environment, so a local test origin keeps working.
`upstream_ca_file` is a PEM bundle that replaces the system roots when an
HTTPS upstream presents a certificate from a private authority; it applies
to the service's own connections and to hydration. Neither setting changes
how clients reach the CDN.

## Client credentials

Your credential helper must provide credentials for the **CDN domain** that
the selected upstream accepts — credentials stored for the upstream host do
not move automatically. Only `Authorization` is forwarded by default,
supporting the upstream's Basic/PAT or Bearer authentication; add Cookie or
other required headers explicitly to `auth_headers`.

With two or more upstreams, distinct `credential_host` values are required
by default. A credential-bearing request with the wrong Host returns 421
before contacting the origin. A shared domain requires explicit
`allow_shared_credential_host: true`, `credential.useHttpPath`, and
path-scoped credentials.

Proxy identity headers such as `X-Forwarded-*` and `X-Remote-User` cannot
be configured as client credentials. The proxy does not persist plaintext
credentials or exchange CDN tokens for upstream identities; credentials
remain in request memory and child-process environments.

## Health and metrics

`GET /healthz` returns health status. `GET /metrics` exports local,
hydration, passthrough and authorization-denial counters, plus cache bytes,
limit, pending reservation bytes, entry count, evictions and evicted bytes.
Metrics also report active requests, admission rejections and temporary
response bytes. Logs omit authentication headers and upstream Git stderr.
