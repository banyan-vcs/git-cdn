# SSH password proxy

Git CDN can terminate SSH and authenticate to a configured Git SSH origin using the client's password. A password can be a regular password or an opaque token accepted by that origin. The CDN does not interpret JWT claims, mint platform tokens or read the client's token file.

## Logging and embedding

The command passes its standard `slog.Logger` to the SSH frontend. Each validated exec emits a completion summary containing repository, command, upstream user, cache status, exit status, duration and stdout byte count. Handshake failures use debug diagnostics. Passwords, packet bodies and origin stderr are never included. A byte count covers protocol output, not only pack bytes; `NONE` means no fetch response path was selected.

Embedded users can pass `WithLogger`, `WithConnContext` and `WithSessionContext` options to `sshproxy.New`. Connection context enrichment runs before the handshake; session enrichment runs for each validated exec and receives `SessionInfo`, without passwords or packet data. Callbacks may run concurrently and must derive from the supplied context to preserve deadlines and cancellation; a nil result keeps the original context. Logger handlers can then associate sessions with application-specific request IDs without adding logging backend dependencies to Git CDN.

This mode applies to Git hosts that accept reusable SSH password credentials. Standard public-key signatures cannot be replayed; key-only services need delegation or a separate signing design. A PAT accepted over HTTPS is not automatically an SSH password. The selected SSH endpoint, port and credential issuer must support the requested mode.

## Configure and run

Start from [config.ssh.example.yaml](../config.ssh.example.yaml). Generate a persistent CDN host key and obtain verified origin host keys before starting:

```sh
mkdir -p ~/.git-cdn
ssh-keygen -t ed25519 -N '' -f ~/.git-cdn/ssh_host_ed25519_key
```

By default, `ssh.upstream.host_key_policy: strict` requires verified origin keys in `~/.git-cdn/known_hosts` and rejects unknown or changed keys. Collecting a key with `ssh-keyscan` alone does not verify its authenticity. Clients should also trust the CDN's host key. The proxy receives reusable credentials and must be trusted to handle them.

```sh
go build -o bin/git-cdn ./cmd/git-cdn
./bin/git-cdn -config config.ssh.example.yaml
```

HTTP and SSH run together. `ssh.listen` defaults to loopback port 2222, `ssh.max_connections` to 16, and `ssh.allowed_users` to `[git]`. Upstream `user` overrides the incoming SSH username; omitting it forwards an allowed incoming username. Each connection accepts at most four session channels. A 15-second handshake bound, the request deadline and the configured write idle timeout bound transport use. Quiet input while local hydration runs is bounded by the total request deadline.

Single mode uses a direct repository path. Multi mode has `ssh.upstreams.<site>` and a leading `site/` path component. `path_prefix` prepends a fixed origin path. Repository case and optional `.git` aliases are preserved and normalized. Both SCP-style and `ssh://` client URLs work; paths are validated and forwarded relative to the configured origin.

When both frontends run in single-upstream mode, SSH shares the HTTP upstream's cache scope by default: objects and response packs are keyed by the HTTP origin URL plus the client-visible repository path, so one repository is cached once for both protocols. Single mode declares that the two endpoints front the same origin; if they do not, split them into multi mode or pin distinct scopes. In multi-site mode, or when `cache_identity_url` is set, SSH keys its own scopes instead: the explicit URL associates SSH with an equivalent HTTP origin — set it only when the configured endpoints and paths represent the same repositories and Git read permission domain — and otherwise objects are keyed by SSH address, upstream username and path, still shared across password values.

## Automatic first-use host-key enrollment

Set `host_key_policy: accept-new` for an origin to record its first observed raw host key during an SSH connection:

```yaml
ssh:
  upstream:
    address: git.example.com:22
    user: git
    known_hosts: ~/.git-cdn/known_hosts
    host_key_policy: accept-new
```

The configured path remains required, but its directory and file can be created automatically. The key is saved before password/token authentication. Failed writes reject the connection. Enrollment uses an atomic file replacement and a separate file lock to preserve existing entries during concurrent connections and processes. Each site in `ssh.upstreams` can select its own policy; the hostname and port identify the trusted endpoint.

`accept-new` trusts the first endpoint observed, so that initial connection must be made over a trusted network. Known keys are checked on every connection. Changed or revoked keys and unusable trust files are rejected; an untrusted host certificate requires a verified CA instead of automatic enrollment. Legitimate key rotation requires independently verifying and updating the saved record. Deleting the file or losing its volume resets first-use trust.

The file and its parent directory must be writable by the service user, since updates also create a lock and temporary file. Put automatic trust under persistent state, such as `/var/lib/git-cdn/known_hosts` in containers. A read-only file bind mount suits `strict`; it cannot be used for automatic updates. `StrictHostKeyChecking=no` in the caller's OpenSSH command only affects the client-to-CDN connection and does not configure this origin policy.

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

The frontend accepts Git upload-pack and receive-pack commands, the LFS commands below, and the `GIT_PROTOCOL` environment request. Shell, PTY, arbitrary commands and forwarding channels are rejected. SSH transport acceptance is not repository authorization: the origin validates the password and selected repository operation before cache access. Invalid passwords, access denial and missing repositories never authorize local objects.

V2 discovery and client ls-refs are relayed. A supported fetch with `done` can use local objects after a fresh, independent origin ls-refs query with the same password. Wanted roots, haves and tags use the existing local eligibility policy. Missing roots are fetched over a new SSH session and received with native `index-pack --fix-thin --strict`, then checked and published through the common cache. Native Git, HTTP, SSH and cache maintenance share the process budget and repository leases.

Completed local responses spool before commitment, preserving fallback after generation failure. The SSH command then closes with a successful exit status, as native upload-pack does after a completed fetch. A negotiation-only fetch switches the remaining session to upstream passthrough; the proxy does not mix origin negotiation state with local state. Ordinary incremental Git fetch may take this path. A later complete clone can hydrate and reuse the updated cache.

Shallow/filter/unknown extensions, unadvertised wants, unsupported haves, SHA-256, v0/v1 and push stay on the origin. Passthrough streams bidirectionally with stderr, EOF and exit status. Session framing is handled separately from the stateless HTTP frontend.

## Git LFS

`git-lfs-authenticate <repository> download|upload` and `git-lfs-transfer <repository> download|upload` are forwarded to the configured SSH origin using the caller's password/token. The operation is required; other operations, extra arguments and unsafe paths are rejected. Both commands use the same repository alias normalization, single/multi-origin routing and optional path prefix as Git commands. The final origin repository path is validated again after routing.

Authentication responses pass through unchanged, including `href`, authorization headers, expiry and extension fields. Returned HTTPS endpoints and object-storage URLs can point outside the CDN. Clients access those URLs directly and need network access to them. The CDN does not mint LFS credentials or rewrite signed object URLs.

Pure SSH LFS transfer uses bidirectional streaming with stdin EOF, stderr and the origin's exit status. The origin must implement `git-lfs-transfer`; hosts that provide only `git-lfs-authenticate` use the hybrid SSH authentication plus HTTP transfer flow. LFS sessions share the existing SSH connection/session limits and total request and write idle deadlines. They do not use local Git pack generation or the Git response spool budget, and LFS objects are not cached. Git packs contain the original LFS pointer blobs.

## Validation

Go integration tests create a password-authenticated local SSH origin and use standard Git/OpenSSH clients. They cover clone, URL alias cache reuse, origin updates, new-root hydration, shallow passthrough, revocation and rejected exec paths. LFS tests cover unchanged authentication JSON, binary passthrough, stderr/EOF/exit status, routing and rejected commands. With a native Git LFS client installed, an offline test verifies clone checkout, push upload and subsequent checkout using external HTTP object URLs. Tests use no external account or token issuer; CI installs the LFS client.

Live origin validation is separate: obtain a credential accepted by the actual SSH endpoint, verify its host key, then compare direct access with clone/fetch through a temporary CDN. Keep actual domains, credentials and generated results in local deployment configuration or ignored artifacts.
