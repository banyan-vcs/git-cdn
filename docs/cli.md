# Command line

The `git-cdn` binary is one command with subcommands: `serve` runs the
daemon, and the operation subcommands drive a running node through its
loopback admin endpoint.

| Command | Purpose |
| --- | --- |
| `git-cdn serve` | Run the CDN daemon (`--config`, `--log-level`) |
| `git-cdn status` | Health, cache and traffic summary |
| `git-cdn scopes` | Cached repository scopes (hash keys, not names) |
| `git-cdn drain` / `git-cdn undrain` | Stop / resume admitting requests |
| `git-cdn reload` | Re-read the configuration file |
| `git-cdn version` | Print the build version |

## Operating a node

The operation subcommands talk to the admin endpoint configured by
`admin_listen` (default `127.0.0.1:8081`, see
[configuration.md](configuration.md#operations-endpoint)). The endpoint is
loopback-only and carries no authentication: it is for an operator on the
node itself, or behind an SSH tunnel to it. Every command takes
`--addr` to point at the endpoint, `--timeout` for the request, and
`--json` for the raw document.

```sh
git-cdn status
git-cdn scopes --sort access --limit 10
```

In Docker the endpoint is inside the container, on the container's own
loopback:

```sh
docker compose exec -T git-cdn git-cdn status
```

## Draining

`drain` marks the node draining: new git requests are refused on both
frontends with a 503 (SSH sessions see the reason on stderr), and
`/healthz` reports the drain so load balancers that probe health stop
routing to the node. A balancer notices on its own probe schedule — with
the default Traefik health-check interval that is tens of seconds, not
instant — so keep draining for at least one probe interval before
relying on the reroute. In-flight transfers keep running. `--wait` polls
until requests and SSH sessions reach zero — a long clone may outlive
`--wait-timeout`; check `status` afterwards.

```sh
git-cdn drain --wait --wait-timeout 5m
systemctl restart git-cdn   # or whatever the deployment uses
```

`undrain` reverses the mark for a node that stays up.

## Reload

`reload` re-reads the YAML the node started from and applies the
reloadable subset live:

- `max_cache_bytes`, `max_pack_cache_bytes`, `max_temp_response_bytes`
- `max_concurrent_requests`, `max_requests_per_second` (the token bucket
  restarts full)
- `max_request_bytes`, `max_refs_bytes`
- `git_timeout`, `fetch_timeout`, `upstream_timeout`,
  `write_idle_timeout`, `request_timeout`
- `auth_probe_cache_ttl` (applies to probes stored after the reload)

Everything else — listeners, upstreams, the SSH frontend, transport and
TLS settings, maintenance tuning — is fixed at construction. When the
file differs from the running node on one of those, the response reports
it by name as restart-required, and keeps reporting it on every reload
until the node restarts into the new file. A file that fails validation
is refused whole: nothing is applied.

```sh
git-cdn reload
applied   max_cache_bytes: 10737418240 -> 5368709120
restart   listen: 127.0.0.1:8080 -> 127.0.0.1:9090
```
