# Local protocol probes

The [probe script](../scripts/protocol-probe/main.go) uses native Git and the Go standard library. It creates temporary repositories, uses no network or personal credentials, and removes its fixtures after execution:

```sh
go run ./scripts/protocol-probe
```

It emits machine-readable results to stdout. Capture those results in an ignored artifact directory when needed, rather than checking a dated snapshot into the documentation.

| Probe | Behavior established | Design implication |
| --- | --- | --- |
| Unadvertised retained OID | v2 can read an object retained locally after the upstream ref disappears, even with allow-SHA1 configuration switches disabled | Fresh ref membership must be checked by the CDN policy |
| Wanted commit with a missing blob | The commit exists, dependency traversal finds the hole, and upload-pack can begin output before failing | Check closure and define the response commitment boundary |
| Exact-OID hydration | Fetching a missing root and its dependencies permits local upload-pack | Request-driven caching does not require a full mirror |
| Shallow client from a complete cache | Native upload-pack emits shallow-info without turning the server cache into a shallow repository | Client depth and server object completeness are different |
| Negotiation-only fetch | v2 can return acknowledgments without a pack | Do not assume every fetch POST is the final pack response |
| Initial v2 advertisement | Capabilities are advertised separately from branch refs | Discovery GET and ls-refs POST are distinct stages |

These probes establish Git primitives, not a complete network service conformance test. The script forces SHA-1. Authentication, HTTP encoding, site routing and real-client behavior are covered by the Go integration tests described in [Testing](testing.md).
