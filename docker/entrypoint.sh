#!/bin/sh
set -eu

if [ "${1:-}" = git-cdn ] && [ "${2:-}" = serve ]; then
  umask 077
  task_key=/var/lib/git-cdn/ssh_host_ed25519_key
  mkdir -p /var/lib/git-cdn/caches
  if [ ! -s "$task_key" ]; then
    task_temp_key="${task_key}.tmp.$$"
    trap 'rm -f "$task_temp_key" "$task_temp_key.pub"' EXIT HUP INT TERM
    ssh-keygen -q -t ed25519 -N '' -f "$task_temp_key"
    mv "$task_temp_key" "$task_key"
    mv "$task_temp_key.pub" "$task_key.pub"
    trap - EXIT HUP INT TERM
  elif [ ! -s "$task_key.pub" ]; then
    ssh-keygen -y -f "$task_key" > "$task_key.pub"
  fi
fi

exec "$@"
