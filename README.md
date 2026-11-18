# Git CDN

English | [简体中文](README.zh-CN.md)

An HTTP Git caching proxy built with Go and native Git. Before serving a fetch locally, it checks the caller's access through upstream `ls-refs`, verifies the requested object graph, and fetches missing roots by exact OID.

The CDN has its own domain, such as `git-cdn.example`. Clients connect to the CDN, which uses the request's upstream credentials to contact the configured Git host.

HTTP smart protocol v2 is implemented. SSH remains a planned feature.

## Run

Requirements: Go 1.24.1+, native Git, and Linux or macOS. Git 2.37+ must support protocol v2, `fetch --no-auto-maintenance` and cruft packs.

```sh
mkdir -p ~/.git-cdn
cp config.example.yaml ~/.git-cdn/config.yaml
go build -o bin/git-cdn ./cmd/git-cdn
./bin/git-cdn
```

The default configuration is `~/.git-cdn/config.yaml`. The default cache directory is `~/.git-cdn/caches`. Override the configuration file with a command-line argument:

```sh
./bin/git-cdn -config ./config.yaml
```

Set another cache directory in YAML:

```yaml
cache_dir: /data/git-cdn/caches
```

Both the configuration path and `cache_dir` expand `~/` to the current user's home directory. Explicit relative paths resolve against the process's startup working directory. Changing a configuration file takes effect after restarting the service.

Non-loopback HTTP listeners require TLS or `trusted_proxy_cidrs`. A trusted proxy must overwrite `X-Forwarded-Proto: https`; untrusted peers cannot bypass the check by supplying this header. Non-loopback upstreams require HTTPS. Use `allow_insecure_http: true` only on a controlled internal network.

The listener defaults to `127.0.0.1:8080`. Run behind a TLS reverse proxy for the CDN domain, or configure both `tls_cert` and `tls_key` to serve HTTPS directly. A front proxy must forward `Git-Protocol` and the configured authentication headers and allow Git request and response bodies.

## Single-site and multi-site modes

Use [config.example.yaml](config.example.yaml) for one upstream. Repository paths map directly to that host:

```yaml
mode: single
upstream:
  url: https://gitlab.com
```

```text
https://git-cdn.example/group/repo
https://git-cdn.example/group/repo.git
    → https://gitlab.com/group/repo.git
```

For multiple upstreams, copy [config.multi.example.yaml](config.multi.example.yaml) to `~/.git-cdn/config.yaml`. The first path segment selects the host:

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

When `mode` is omitted, `upstream` selects single-site mode and `upstreams` selects multi-site mode. A one-entry `upstreams` map still uses a site prefix. Mixing both fields, unknown fields, duplicate keys, and multiple YAML documents are rejected at startup.

Both modes preserve path case and normalize URLs with or without `.git` to the same upstream path and cache identity. Upstream URLs may include a base path. Discovery, ls-refs, fetch, push, and standard `info/lfs` routes normalize the suffix before forwarding POST requests. Redirects retain the direct repository path in single-site mode and the site prefix in multi-site mode.

## Client setup

Clone through a single-site CDN:

```sh
git clone https://git-cdn.example/group/repo.git
```

Or configure URL rewriting:

```sh
git config --global url."https://git-cdn.example/".insteadOf "https://gitlab.com/"
```

In multi-site mode, use `/gitlab/group/repo.git` and the rewrite prefix `url."https://git-cdn-gitlab.example/gitlab/".insteadOf`. Configure each upstream prefix separately.

This rule also affects push. The CDN forwards receive-pack discovery and push requests to the upstream. Per-remote fetch/push URLs can instead route fetch through the CDN and push directly to the host; see the design document for their interaction with global rewriting.

Your credential helper must provide credentials for the **CDN domain** that the selected upstream accepts. Credentials stored for the upstream host do not move automatically. Only `Authorization` is forwarded by default, supporting the upstream's Basic/PAT or Bearer authentication. Add Cookie or other required authentication headers explicitly to `auth_headers`. With two or more upstreams, distinct `credential_host` values are required by default. A credential-bearing request with the wrong Host returns 421 before contacting the origin. A shared domain requires explicit `allow_shared_credential_host: true`, `credential.useHttpPath`, and path-scoped credentials. Proxy identity headers such as `X-Forwarded-*` and `X-Remote-User` cannot be configured as client credentials. The proxy does not persist plaintext credentials or exchange CDN tokens for upstream identities.

## Request behavior

| Request | Behavior |
| --- | --- |
| Discovery and ls-refs | Live upstream forwarding, preserving capabilities and refs |
| HTTP v2 fetch for a fresh tip, tag, or peeled OID | Authorize, verify dependencies, hydrate exact roots if needed, then run local upload-pack |
| Ordinary have negotiation | Haves must be ancestor commits of authorized wanted roots; otherwise forward upstream |
| include-tag | Check current tag relationships, hydrate relevant tags, and use a request-specific ref view |
| Shallow, filter, want-ref, other historical/blob/tree OIDs, unknown extensions, SHA-256, v0/v1 | Forward upstream |
| Receive-pack, push, and Git LFS repository endpoints | Forward upstream without object caching |

Every fetch selected for local execution issues its own ls-refs request, including direct POSTs without an earlier discovery request. Upstream 401/403/404 responses preserve status and authentication challenges. Login pages or invalid ref responses cannot authorize cached data.

GitHub's Git endpoint also returns 401 for nonexistent repositories. If its public owner API confirms the owner is missing, the CDN returns 404 without a login challenge. An existing owner, failed/rate-limited metadata lookup, or unknown repository visibility preserves 401; a repository API 404 alone can conceal a private repository and is not treated as proof of absence. Other hosts retain their upstream status.

Object stores are shared by **upstream URL, canonical repository path and object format**, at `repos/<repository-hash>/repo.git`. Different users and rotated tokens reuse the same objects. Every local fetch still uses its own credentials for live authorization and missing-object hydration; credentials remain in request memory and child-process environments. Shared storage assumes repository-level Git read access. Repository operations are locked. Each cache directory can be owned by only one service process; multiple instances use separate directories. Startup merges legacy credential directories into the shared repository and removes the obsolete scope key after validation.

Objects are received and checked in a staging bare repository before pack/index files and GC roots are published. Dependency traversal detects missing blobs even when the wanted commit exists. Healthy cached roots participate in incremental negotiation through alternates. Cached refs are never used as the client's upstream ref view.

An unfiltered v2 fetch with wants, `done` and no haves reconciles stored refs against a complete live ls-refs snapshot. This includes ordinary clones and fetches into empty repositories; Git does not label clone operations on the wire. Complete cached tips are recorded under `refs/cache/upstream/`, obsolete names and root pins are removed, and HEAD is updated. Unrequested branches are not downloaded. Incremental, shallow, filtered and negotiation-only requests do not prune refs.

Local responses are written to a size-limited temporary file. After Git exits successfully, the server sends Content-Length and cache status headers and streams that file. Generation failures, timeouts, and size overruns fall back before committing the response. This increases local time to first byte because generation must finish before transfer. A pack-objects output cache is not implemented yet.

`X-GitCDN-Cache-Status` values: `HIT` means required objects were already complete, `MISS` means they were hydrated before local generation, and `BYPASS` means an upstream response. HIT refers to objects, not a reused response pack.

## Configuration and limits

| Setting | Default | Purpose |
| --- | --- | --- |
| `cache_dir` | `~/.git-cdn/caches` | Persistent objects, staging, response files, and the process lock |
| `max_cache_bytes` | 20 GiB | Total logical file bytes across persistent repository cache scopes; idle scopes are evicted by LRU |
| `repack_pack_threshold` | 32 | Queue idle repositories at or above this pack count; minimum 2 |
| `repack_timeout` | 2m | Deadline for background repack and GC |
| `gc_interval` | 24h | Interval between successful garbage collections, independent of pack count |
| `gc_retention` | 336h | Minimum 14-day window for recent unreachable objects and removed-root histories |
| `mode` | Inferred from upstream/upstreams | Direct paths for single; site-prefixed paths for multi |
| `upstream` | Required for single | One fixed host with a `url` field |
| `upstreams` | Required for multi | Site ID to upstream mapping |
| `max_request_bytes` | 8 MiB | Compressed and decompressed v2 upload-pack POST limit |
| `max_refs_bytes` | 16 MiB | Internal ls-refs response limit |
| `max_response_bytes` | 1 GiB | Local response spool limit; larger responses fall back |
| `max_concurrent_git` | 4 | Native Git process concurrency |
| `max_concurrent_requests` | 16 | Active Git requests, held through parsing, passthrough and response transfer |
| `max_requests_per_second` | 64 | Instance-wide rate, with an initial burst of the same size |
| `max_temp_response_bytes` | 4 GiB | Total bytes in unfinished local responses, released after file deletion |
| `write_idle_timeout` | 30s | Deadline for each write/flush; progressing transfers refresh it |
| `request_timeout` | 10m | Total Git request deadline, including input, origin access and transfer |
| `fetch_timeout` | 2m | Deadline for each hydration operation |
| `git_timeout` | 2m | Total local deadline, including queueing, validation, hydration, and packing |
| `upstream_timeout` | 30s | Deadline for the live ref probe |

Each v2 command accepts at most 4096 packets without materializing a packet array. The local path handles up to 64 wants, 128 haves, and 128 current tags for include-tag requests. Larger requests fall back. Gzip v2 requests are decoded for inspection and retain their original compressed body when forwarded. Other encodings are forwarded. Canonical redirects within the trusted upstream base path are rewritten to the CDN.

Persistent repository caches use LRU eviction with a default 20 GiB budget. Access times survive restarts, and startup trims existing caches if the configured budget is smaller. Before publication, the server reserves room and removes the oldest idle repositories. Active leases and waiting requests pin their repositories, so hydration, alternates, and packing cannot race with eviction. A repository larger than the budget, or a budget fully pinned by active requests, falls back upstream. Evicted repositories are fetched again on demand.

Idle repositories with enough packs run `git repack --geometric=2 -d` under an exclusive lease and the global Git process limit. Maintenance builds and validates a private pack set using hardlinks to immutable objects, reserves extra capacity and checks new-file growth every 200 ms before publishing. Failure keeps the existing packs. Insufficient capacity skips repack; it does not fail a client response. Geometric repack consolidates normal packs while preserving cruft age metadata; timed GC is described below. Metrics expose completed, failed and skipped repacks.

GC runs on idle repositories at `gc_interval`, including repositories below the repack threshold. It uses `git repack -a -d --cruft --cruft-expiration=<cutoff>` and expired-loose-object pruning in the same private staging/publishing workflow. `gc_retention` defaults to 14 days and cannot be configured shorter. Git's object mtimes and cruft `.mtimes` retain recent unreachable objects and their dependencies; geometric repack keeps cruft packs separate so their ages survive. Removed root histories additionally receive `refs/cache/retained/<oid>` grace pins for at least 14 days from unpinning, tracked in `.gc-retained`. These pins never authorize client reads. `.gc-last` persists the last successful collection. Activity and queued leases block GC; budget/time failures retain published objects. LRU still evicts whole idle repositories as recoverable caches and is independent of the GC retention window.

```yaml
max_cache_bytes: 21474836480 # 20 GiB
```

The budget counts regular-file lengths under managed `repos/` scopes, including Git metadata. It excludes directory/block overhead, the process lock and `tmp/`. Each staging download is checked against the same limit before publication and every 200 ms while fetching; temporary writes can exceed it between checks. Response files use `max_response_bytes` per file and `max_temp_response_bytes` globally; reservations remain held until file deletion. Startup clears orphaned service temporary files under the directory lock. Concurrent staging and response files therefore require additional disk space: this is a persistent-cache admission limit, not a filesystem quota on the entire cache directory. Background warming and shared cache coordination across nodes are not implemented. The service owns its cache repositories; do not prune or delete objects while it is running.

Stable upstream repository IDs are not checked yet. Invalidate a path's old cache when the upstream deletes and recreates that repository. Local HTTP v2 acceleration requires upstream v2 support. Verify the selected host's protocol capabilities and credential behavior during deployment.

Git requests exceeding concurrency or rate limits return 503 with `Retry-After: 1` before input is read. Health and metrics do not consume Git request slots. See [HTTP security](docs/http-security.md) for transport requirements, credential host binding and configuration migration.

`GET /healthz` returns health status. `GET /metrics` exports local, hydration, passthrough, and authorization-denial counters, plus cache bytes, limit, pending reservation bytes, entry count, evictions and evicted bytes. Metrics also report active requests, admission rejections and temporary response bytes. Logs omit authentication headers and upstream Git stderr.

## Validation

```sh
go test ./...
go test -race ./...
go vet ./...
```

Integration tests run an authenticated native `git http-backend` and standard Git clients. They cover clone, incremental fetch, push, YAML validation, both modes and URL suffixes, cache reuse, per-site authorization, stale OID forwarding, missing blob/pack repair, concurrent misses, timeouts, response limits, gzip, and redirects. The tests have passed with Git 2.54.0 and 2.42.0, race detection, and vet.

Cache policy tests cover LRU order, restart recovery, smaller startup budgets, active/waiting lease protection, concurrent capacity reservations, oversized fallback, refetch after eviction, and cancellation of growing staging data. GitHub response tests cover confirmed missing owners, preserved authentication for unknown visibility, and metadata failures.

See [Testing](docs/testing.md) for protocol coverage, optional live validation and performance measurement. With a multi-site service running and its binary built, select a public repository explicitly:

```sh
go run ./scripts/live-repository --repository git/git --concurrency 6
```

The script contacts GitHub, retains results and client repositories under `bin/live-tests/`, and stops its temporary services. Repositories such as `git/git` and `redis/redis` provide recognizable workloads. Single-run durations are observations, not a performance baseline; keep generated reports out of source control.

The protocol parser also has a fuzz target:

```sh
go test ./internal/protocol -fuzz=FuzzParseFetch -fuzztime=10s -parallel=4
```

## Design and protocol evidence

- [Design](docs/design.md): architecture, authorization boundaries, cache consistency and performance tradeoffs.
- [Cache policy](docs/cache-policy.md): size admission, LRU, lease protection and GitHub status handling.
- [Protocol probes](docs/protocol-probes.md): local Git checks and their implications.
- [Probe script](scripts/protocol-probe/main.go): Go standard library and native Git; no network or personal credentials.

SSH will require platform delegation, a configured user credential mapping, or explicit agent forwarding. See the design document.

## License

MIT. See [LICENSE](LICENSE). Third-party dependencies retain their own licenses.
