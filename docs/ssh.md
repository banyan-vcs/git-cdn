# SSH password proxy

Git CDN can terminate SSH and authenticate to a configured Git SSH origin using the client's password. A password can be a regular password or an opaque token accepted by that origin. The CDN does not interpret JWT claims, mint platform tokens or read the client's token file.

This mode applies to Git hosts that accept reusable SSH password credentials. Standard public-key signatures cannot be replayed; key-only services need delegation or a separate signing design. A PAT accepted over HTTPS is not automatically an SSH password. The selected SSH endpoint, port and credential issuer must support the requested mode.

## Configure and run

Start from [config.ssh.example.yaml](../config.ssh.example.yaml). Generate a persistent CDN host key and obtain verified origin host keys before starting:

```sh
mkdir -p ~/.git-cdn
ssh-keygen -t ed25519 -N '' -f ~/.git-cdn/ssh_host_ed25519_key
```

Store verified origin keys in `~/.git-cdn/known_hosts`. The proxy requires this file and rejects mismatches; collecting a key with `ssh-keyscan` alone does not verify its authenticity. Clients should also trust the CDN's host key. The proxy receives reusable credentials and must be trusted to handle them.

```sh
go build -o bin/git-cdn ./cmd/git-cdn
./bin/git-cdn -config config.ssh.example.yaml
```

HTTP and SSH run together. `ssh.listen` defaults to loopback port 2222, `ssh.max_connections` to 16, and `ssh.allowed_users` to `[git]`. Upstream `user` overrides the incoming SSH username; omitting it forwards an allowed incoming username. Each connection accepts at most four session channels. A 15-second handshake bound, the request deadline and the configured write idle timeout bound transport use. Quiet input while local hydration runs is bounded by the total request deadline.

Single mode uses a direct repository path. Multi mode has `ssh.upstreams.<site>` and a leading `site/` path component. `path_prefix` prepends a fixed origin path. Repository case and optional `.git` aliases are preserved and normalized. Both SCP-style and `ssh://` client URLs work; paths are validated and forwarded relative to the configured origin.

`cache_identity_url` explicitly associates SSH with the equivalent HTTP origin URL. Set it only when the configured endpoints and paths represent the same repositories and Git read permission domain. Without this setting SSH objects are keyed by SSH address, upstream username and path, and still shared across password values.

## Client credentials

The SSH server offers only `password` authentication and sends an authentication banner explaining that public-key authentication is unsupported. Clients configured to use only SSH keys are rejected during authentication, before any origin connection or cache access. Use a password/token accepted by the origin, or connect directly to the origin for public-key authentication.

Clients that have both keys and a password can still authenticate with the password. The banner is sent once per connection, including successful password connections. SSH client libraries need a banner callback to display this message; OpenSSH displays it by default.

For a token file, an optional native `sshpass` client can read the file without exposing the token in command arguments:

```sh
GIT_SSH_COMMAND='sshpass -f ~/.git-cdn/password ssh -p 2222' \
  git clone git@cdn.example:owner/repository.git
```

SSH libraries can supply the password directly; neither the server nor the Go implementation depends on `sshpass`. No token issuer or platform-specific domain is built into Git CDN. The caller owns credential retrieval and refresh. An already established SSH connection cannot replace its password when a file changes; reconnect after renewal or expiry.

## Git behavior

The frontend accepts Git upload-pack and receive-pack exec commands and the `GIT_PROTOCOL` environment request. Shell, PTY, arbitrary commands and forwarding channels are rejected. SSH transport acceptance is not repository authorization: the origin validates the password and selected repository operation before cache access. Invalid passwords, access denial and missing repositories never authorize local objects.

V2 discovery and client ls-refs are relayed. A supported fetch with `done` can use local objects after a fresh, independent origin ls-refs query with the same password. Wanted roots, haves and tags use the existing local eligibility policy. Missing roots are fetched over a new SSH session and received with native `index-pack --fix-thin --strict`, then checked and published through the common cache. Native Git, HTTP, SSH and cache maintenance share the process budget and repository leases.

Completed local responses spool before commitment, preserving fallback after generation failure. The SSH command then closes with a successful exit status, as native upload-pack does after a completed fetch. A negotiation-only fetch switches the remaining session to upstream passthrough; the proxy does not mix origin negotiation state with local state. Ordinary incremental Git fetch may take this path. A later complete clone can hydrate and reuse the updated cache.

Shallow/filter/unknown extensions, unadvertised wants, unsupported haves, SHA-256, v0/v1 and push stay on the origin. Passthrough streams bidirectionally with stderr, EOF and exit status. Session framing is handled separately from the stateless HTTP frontend.

## Validation

Go integration tests create a password-authenticated local SSH origin and use standard Git/OpenSSH clients. They cover clone, URL alias cache reuse, origin updates, new-root hydration, shallow passthrough, revocation and rejected exec paths. Tests require native Git and OpenSSH, but no external account or token issuer.

Live origin validation is separate: obtain a credential accepted by the actual SSH endpoint, verify its host key, then compare direct access with clone/fetch through a temporary CDN. Keep actual domains, credentials and generated results in local deployment configuration or ignored artifacts.
