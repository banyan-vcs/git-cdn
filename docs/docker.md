# Docker Compose quickstart

The multi-stage image contains Git CDN, native Git, SSH tools, CA certificates and the MIT license. Compose runs as UID/GID 10001, mounts configuration read-only and stores cache plus SSH identity in a named volume.

## HTTP

With Docker Engine and Compose v2+ running:

```sh
docker compose up -d --build --wait
curl http://127.0.0.1:8080/healthz
git clone http://127.0.0.1:8080/github/owner/repository.git
```

[docker/config.yaml](../docker/config.yaml) proxies GitHub and GitLab using `/github/owner/repository[.git]` and `/gitlab/group/repository[.git]`. Host ports bind `127.0.0.1`. The local profile explicitly enables backend HTTP and a shared credential hostname. Production uses HTTPS and the transport/credential-host rules in [HTTP security](http-security.md).

Private repository credentials belong to the selected origin path:

```sh
git config --global credential.http://127.0.0.1:8080.useHttpPath true
```

Configure the credential helper per site. The CDN does not issue upstream credentials.

## Configuration

Use a private copy for custom sites, single-site mode or cache budgets:

```sh
cp docker/config.yaml docker/config.local.yaml
GIT_CDN_CONFIG=./docker/config.local.yaml docker compose up -d --force-recreate --wait
```

Keep `listen: 0.0.0.0:8080` and `cache_dir: /var/lib/git-cdn/caches` inside the container. `GIT_CDN_HTTP_PORT` changes the loopback host port. YAML changes require recreating/restarting the service. The image runs `git-cdn -config /etc/git-cdn/config.yaml`; Compose's `command` can override daemon arguments.

```sh
GIT_CDN_HTTP_PORT=18080 docker compose up -d --wait
docker compose build --build-arg GOPROXY=https://proxy.golang.org,direct
```

Docker build args `GO_IMAGE` and `RUNTIME_IMAGE` select alternative base images. The default build targets the container platform.

## Optional SSH

Configure a real password/token-capable origin in [docker/config.ssh.yaml](../docker/config.ssh.yaml). Ordinary key-only SSH services need a separate signing/delegation design; see [SSH requirements](ssh.md). Put verified origin keys in `docker/known_hosts`, or select another file through `GIT_CDN_KNOWN_HOSTS`. These public keys must be readable by the container user; `chmod 644 docker/known_hosts` is appropriate. A missing bind source is rejected rather than created as a directory.

The supplied SSH override uses `host_key_policy: strict` and a read-only origin trust file. For `accept-new`, use a local copy of the SSH override that removes the `/etc/git-cdn/known_hosts` bind mount, and select a custom SSH YAML with `host_key_policy: accept-new` and `known_hosts: /var/lib/git-cdn/known_hosts`. The existing state volume provides the writable persistent directory. The service records the first observed origin key and rejects subsequent changes; see [first-use trust](ssh.md#automatic-first-use-host-key-enrollment).

```sh
docker compose -f docker-compose.yml -f docker-compose.ssh.yml up -d --build --wait
docker compose exec -T git-cdn cat /var/lib/git-cdn/ssh_host_ed25519_key.pub
```

The entrypoint generates an ed25519 CDN host key atomically on first start and reuses it from the state volume. Trust its public key on the client. No private key is included in the image. SSH binds container port 2222; `GIT_CDN_SSH_PORT` changes the loopback host port. `GIT_CDN_SSH_CONFIG` selects another SSH-enabled YAML.

```sh
GIT_SSH_COMMAND='sshpass -f ~/.git-cdn/password ssh -p 2222' \
  git clone git@127.0.0.1:owner/repository.git
```

The client owns the password/token file. The proxy receives its value over SSH and does not mount or read that file.

## Cluster example

[examples/cluster](../examples/cluster) scales the same image out behind one load balancer: HTTP and SSH leave the same DNS name, each instance keeps its own cache volume, and the SSH host key is baked into the image so every instance presents one identity. A demo origin with password SSH and anonymous HTTP makes both paths verifiable offline, and an optional metrics profile brings Prometheus scraping every instance. See the example's README for the topology, the offline variant and the key lifecycle.

## State and operations

`git-cdn-data` mounts `/var/lib/git-cdn`: `caches/` holds repository objects and temporary staging, and the persistent SSH host key lives beside it. Restart, rebuild and ordinary Compose down preserve this volume. YAML controls LRU admission, geometric repack and 14-day GC retention; temporary staging needs additional space beyond the persistent budget.

```sh
docker compose ps
docker compose logs -f git-cdn
docker compose exec -T git-cdn git --version
docker compose down
```

`docker compose down -v` deletes cache and SSH identity; use it only for an intentional reset. Clients then need to trust the replacement host key. Multiple replicas cannot use the same cache volume because it has an exclusive process lock. Bind-mounted state requires permissions for UID/GID 10001.

Health is checked at `/healthz`. The root filesystem is read-only, `/tmp` is tmpfs, and the init/exec entrypoint forwards shutdown signals. Local config and credential files are excluded from the build context.
