# HTTP security and resource limits

The HTTP frontend terminates client transport and receives upstream credentials. Production clients must use HTTPS and trust the CDN operator. These protections cover Git reads, unsupported protocol passthrough and pushes.

## Admission and parsing

`max_concurrent_requests` defaults to 16. A request holds its slot from before body parsing until its response handler finishes, including while waiting for Git, contacting the origin or sending a local response. `max_requests_per_second` defaults to 64, with a burst of 64 and continuous refill. Limits are per process. Overload returns 503 with `Retry-After: 1` before reading Git input. Health endpoints and metrics do not consume Git admission slots.

The 8 MiB compressed/decompressed upload-pack request limit remains. The parser processes packets individually, rejects invalid framing immediately, and accepts at most 4096 packets per command. It retains at most 64 wants and 128 haves; larger selections within the packet limit bypass local execution with the original request preserved. Upstream refs use a separate streaming parser and the configured byte bound.

`request_timeout` defaults to 10 minutes and includes input, origin access and response transfer. Cancellation interrupts network reads/writes and origin requests. `write_idle_timeout` defaults to 30 seconds for each output write or flush. Progress refreshes the deadline, so it is not a 30-second total download limit. Native Git deadlines remain separate. Origin connections are bounded per host by the request concurrency limit.

## Response spools and restart recovery

`max_response_bytes` limits one local response to 1 GiB by default. `max_temp_response_bytes` defaults to 4 GiB and limits all unfinished local response files together. Writers reserve capacity before each write; reservations remain charged during client transfer and are released only after file removal. If generation exceeds either budget, the partial file is removed and the original request goes to the origin before local output is committed. Failed removal remains charged until restart.

Startup holds the exclusive cache directory lock and removes orphaned files and directories under the service-owned `tmp/` directory. Persistent repository objects remain intact. Do not store unrelated files under `tmp/`.

This is a response spool budget, not a filesystem quota. Staging retains its per-stage `max_cache_bytes` check every 200 ms and needs additional space. Provision disk for persistent objects, concurrent staging, response spools and filesystem overhead.

Metrics add `gitcdn_active_requests`, `gitcdn_rejected_requests_total`, `gitcdn_temp_response_bytes` and `gitcdn_temp_response_max_bytes`. Git passthrough responses also use `Cache-Control: no-store` so a front HTTP cache does not replace application authorization.

## Transport configuration

Loopback HTTP remains available for development and a same-machine TLS proxy. Other upstream HTTP URLs require explicit `allow_insecure_http: true`; the default requires HTTPS.

A non-loopback listener requires local TLS, configured trusted proxy CIDRs, or explicit `allow_insecure_http`. For a TLS proxy on another host:

```yaml
listen: ":8080"
trusted_proxy_cidrs: ["10.20.30.0/24"] # Replace with the actual proxy addresses.
```

The proxy must preserve the CDN Host, Git paths, `Git-Protocol` and configured credential headers. It must overwrite `X-Forwarded-Proto` with a single `https` value. Plaintext requests from untrusted non-loopback peers are rejected even when they supply that header. Restrict network access to the intended proxy. `allow_insecure_http` is an explicit opt-out for controlled private transport.

Proxy identity headers such as `X-Forwarded-*`, `X-Remote-User`, `X-Authenticated-User` and `X-Real-IP` are rejected in `auth_headers`. The service forwards caller credentials and does not support client-supplied trusted identity assertions.

## Multi-upstream credentials

With two or more upstreams and credential forwarding enabled, the default requires a distinct `credential_host` for each upstream:

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

Point both CDN hostnames at the service and configure their TLS certificates. Use `https://git-cdn-gitlab.example/gitlab/group/repo.git` and `https://git-cdn-github.example/github/owner/repo.git`. A credential-bearing request whose Host does not match the selected upstream returns 421 before any origin request. Matching is case-insensitive and includes an explicitly configured port. Hosts must use distinct hostnames even when ports differ.

Existing shared-domain installations can set `allow_shared_credential_host: true`. Configure path-scoped credentials before using that mode:

```sh
git config --global credential.https://git-cdn.example.useHttpPath true
```

Configure the helper separately for `gitlab/` and `github/`. A helper returning one token for every path defeats client-side isolation. Cookie credentials must likewise be scoped to the intended site path. The server cannot infer the intended issuer of an arbitrary caller-supplied token in shared-domain mode. Distinct custom domains can use default host binding.

## Verification

Security tests cover the unauthenticated packet flood, bounded object selection, retained response files, exhausted spool fallback, slot/rate limits, orphan cleanup, proxy checks and prevention of forwarding credentials to the wrong upstream. Real TCP tests cover stalled output and progressing transfers with connection reuse. Existing tests cover authorization revocation, credential isolation, clone/fetch/push and cache recovery.
