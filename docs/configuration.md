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

## Request limits

Each v2 command accepts at most 4096 pkt-lines without materializing a
packet array. The local path handles up to 64 wants, 128 haves and 128
current tags for include-tag requests; larger requests fall back upstream.
Gzip v2 requests are decoded for inspection and forwarded with their
original compressed body; other encodings forward as-is. Canonical
redirects within the trusted upstream base path are rewritten to the CDN.
Git requests exceeding concurrency or rate limits return 503 with
`Retry-After: 1` before input is read; health and metrics requests do not
consume Git request slots.

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
