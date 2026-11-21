# Core request logging

The command writes structured JSON to stderr. INFO is the default; use `-log-level DEBUG` for native Git and queue diagnostics. Embedded users supply a standard `slog.Logger`; request contexts, including application correlation IDs, remain attached to events. The core has no platform logging dependency.

| Event | Level | Useful fields |
| --- | --- | --- |
| `repository request completed` | INFO, WARN for server errors | transport, method, upstream, upstream_host, repository, endpoint, HTTP status, response_bytes, duration_ms, cache_status, pack_cache_status, auth_cache_status |
| `SSH session completed` | INFO, WARN for failed exec | repository, command, upstream_user, cache_status, pack_cache_hit, exit_status, response_bytes, duration_ms |
| `upstream refs refreshed` / `upstream SSH refs refreshed` | INFO | refs phase, status for HTTP, ref_count, duration_ms |
| `upstream passthrough completed` | INFO | reason, upstream status, upstream_response_bytes, duration_ms |
| `cache hydration started` / `cache objects hydrated` | INFO | cache_key, root_count, phase, duration_ms |
| `cache objects reused` / `repository lease acquired` | DEBUG | cache_key, root_count or queue_wait_ms |
| `local fetch prepared` | INFO | cache_key, want_count, have_count, cache_status, pack_cache_hit, response_bytes, duration_ms |
| `local pack generated` | INFO | cache_key, response_bytes, duration_ms, pack_store_result (STORED/SKIPPED/FAILED/DISABLED) |
| `authorization probe completed` | DEBUG | probe_result (LIVE/MERGED/HIT), auth_cache_ttl_ms, ref_count, duration_ms, status for relayed responses |
| `response pack generation wait completed` / `canceled` / `retried` | DEBUG | cache_key, queue_wait_ms, safe error fields or retry reason |
| `response pack stream selected` / `response broadcast backpressure` | DEBUG | cache_key, stream_mode (broadcast/file), response_bytes or subscriber_queue_full reason |
| `cache refs reconciled` | INFO when changed, otherwise DEBUG | cache_key, ref_count, roots_retired, changed, duration_ms; operation=reconcile for background work |
| `response pack cache entry invalidated` / `response pack cache pruned` | WARN / INFO | cache_key, size_mismatch and expected/actual bytes, or entries_evicted/evicted_bytes and remaining budget |
| `cache maintenance completed` | INFO, DEBUG when unchanged | cache_key, operation (repack/gc), changed, duration_ms |
| `native Git command completed` | DEBUG | command, success, queue_wait_ms, duration_ms, exit_code on failure |
| Failure events | WARN, DEBUG for cancellation | phase, error_kind, error_reason when known, exit_code, network_op, filesystem_op, errno, system_error |

`cache_status=HIT` means a local response was served without hydration, either by reusing complete objects or a stored response pack. `MISS` means objects were hydrated before local generation. `pack_cache_hit=true` explicitly identifies verbatim response-pack reuse; the HTTP summary also mirrors the pack-cache header as `pack_cache_status=HIT/MISS/NONE`. `NONE` means the header was absent, including disabled caching and bypass. An enabled but ineligible incremental request carries `MISS`, matching the existing header. `BYPASS` streams the upstream response; the reason identifies protocol policy or local fallback. `cache_status=NONE` means no local/passthrough response was selected. HTTP response bytes are transmitted bytes; passthrough upstream bytes count what was read from the origin. SSH summary bytes include Git protocol output. Health and metrics polling do not emit repository access summaries.

HTTP `auth_cache_status=LIVE` includes a fresh probe shared by concurrent callers; `HIT` means successful snapshot reuse within the explicitly configured `auth_probe_cache_ttl`. The default lifetime is zero, so every request uses live authorization. DEBUG probe events distinguish LIVE, MERGED and HIT, never include the credential-derived probe key, and keep each caller's log context. A shared upstream probe retains its leader's correlation values while using its own timeout and surviving leader cancellation.

Normal object-completeness probes can fail on a cold cache and remain DEBUG. A failed hydration or local response records its phase before fallback, so a successful upstream response does not hide the local failure. Reconciliation preempted by a request remains DEBUG; other reconciliation failures use WARN. Maintenance, pack-file validation/pruning and broadcast backpressure events use the opaque cache key because shared/background work has no single current caller or permission snapshot. Broadcast backpressure means a subscriber falls back to private file reads; the transfer can still succeed.

Passwords, authentication headers, URL query/userinfo, command arguments, packet bodies and raw Git stderr are not logged. Error diagnostics inspect known error types rather than formatting arbitrary upstream errors. Filesystem paths and network addresses inside errors are omitted; configured origin hosts and validated repository paths identify the operation. An SSH authentication/host-key failure preserves a safe client-facing message while retaining its cause for classification.
