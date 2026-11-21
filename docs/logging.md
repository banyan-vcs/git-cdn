# Core request logging

The command writes structured JSON to stderr. INFO is the default; use `-log-level DEBUG` for native Git and queue diagnostics. Embedded users supply a standard `slog.Logger`; request contexts, including application correlation IDs, remain attached to events. The core has no platform logging dependency.

| Event | Level | Useful fields |
| --- | --- | --- |
| `repository request completed` | INFO, WARN for server errors | transport, method, upstream, upstream_host, repository, endpoint, HTTP status, response_bytes, duration_ms, cache_status |
| `upstream refs refreshed` / `upstream SSH refs refreshed` | INFO | refs phase, status for HTTP, ref_count, duration_ms |
| `upstream passthrough completed` | INFO | reason, upstream status, upstream_response_bytes, duration_ms |
| `cache hydration started` / `cache objects hydrated` | INFO | cache_key, root_count, phase, duration_ms |
| `cache objects reused` / `repository lease acquired` | DEBUG | cache_key, root_count or queue_wait_ms |
| `local fetch prepared` | INFO | cache_key, want_count, have_count, cache_status, response_bytes, duration_ms |
| `cache maintenance completed` | INFO, DEBUG when unchanged | cache_key, operation (repack/gc), changed, duration_ms |
| `native Git command completed` | DEBUG | command, success, queue_wait_ms, duration_ms, exit_code on failure |
| Failure events | WARN, DEBUG for cancellation | phase, error_kind, error_reason when known, exit_code, network_op, filesystem_op, errno, system_error |

`HIT` means complete reusable objects, not reuse of a previously generated response pack. `MISS` means objects were hydrated before local pack generation. `BYPASS` streams the upstream response; the reason identifies protocol policy or local fallback. `NONE` means no local/passthrough cache response was selected. HTTP response bytes are transmitted bytes; passthrough upstream bytes count what was read from the origin. SSH summary bytes include Git protocol output. Health and metrics polling do not emit repository access summaries.

Normal object-completeness probes can fail on a cold cache and remain DEBUG. A failed hydration or local response records its phase before fallback, so a successful upstream response does not hide the local failure. Maintenance events use the opaque cache key because background work has no current caller or permission snapshot.

Passwords, authentication headers, URL query/userinfo, command arguments, packet bodies and raw Git stderr are not logged. Error diagnostics inspect known error types rather than formatting arbitrary upstream errors. Filesystem paths and network addresses inside errors are omitted; configured origin hosts and validated repository paths identify the operation. An SSH authentication/host-key failure preserves a safe client-facing message while retaining its cause for classification.
