#!/bin/sh
# Regenerate the cluster key material: the node host key baked into the
# node image, the demo origin's host key, the node image's known_hosts
# pinning the origin, and the client known_hosts trusting the node key.
# Run once from this directory; rebuilds then keep the same identities.
# Everything this script writes is gitignored.
set -eu
cd "$(dirname "$0")"

mkdir -p keys/node keys/origin
rm -f keys/node/ssh_host_ed25519_key keys/node/ssh_host_ed25519_key.pub
rm -f keys/origin/ssh_host_ed25519_key keys/origin/ssh_host_ed25519_key.pub
ssh-keygen -q -t ed25519 -N '' -C 'git-cdn-cluster node' -f keys/node/ssh_host_ed25519_key
ssh-keygen -q -t ed25519 -N '' -C 'git-cdn-cluster origin' -f keys/origin/ssh_host_ed25519_key

origin_pub=$(cat keys/origin/ssh_host_ed25519_key.pub)
printf '%s %s\n%s %s\n' origin "$origin_pub" '[origin]:22' "$origin_pub" > origin_known_hosts

# Port 22 is the SSH default: its known_hosts lines use the bare hostname,
# the bracketed form is for the non-default ports only.
node_pub=$(cat keys/node/ssh_host_ed25519_key.pub)
printf '%s %s\n%s %s\n%s %s\n%s %s\n' \
  '[127.0.0.1]:2222' "$node_pub" '[localhost]:2222' "$node_pub" \
  '[git-cdn.example.com]:22' "$node_pub" 'git-cdn.example.com' "$node_pub" \
  > client_known_hosts

echo "node:   $(ssh-keygen -lf keys/node/ssh_host_ed25519_key.pub)"
echo "origin: $(ssh-keygen -lf keys/origin/ssh_host_ed25519_key.pub)"
