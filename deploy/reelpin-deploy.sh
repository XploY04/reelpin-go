#!/usr/bin/env bash
# Replace one service with a new image digest, on a host that already has its
# env file and its systemd unit.
#
#   reelpin-deploy.sh api dev ghcr.io/xploy04/reelpin-go@sha256:...
#
# The unit is expected to be `reelpin-<service>-<environment>` and to run the
# container named the same. The env file is /etc/reelpin/<environment>.env and
# is never written by this script: secrets belong to the host.
set -euo pipefail

service="${1:?usage: reelpin-deploy.sh <api|worker> <environment> <image@digest>}"
environment="${2:?missing environment}"
image="${3:?missing image digest}"

case "$service" in
  api|worker) ;;
  *) echo "unknown service $service" >&2; exit 1 ;;
esac

# A digest, not a tag. A tag can move between the dev deploy and this one.
if [[ "$image" != *"@sha256:"* ]]; then
  echo "refusing to deploy $image: pass an image digest, not a tag" >&2
  exit 1
fi

env_file="/etc/reelpin/${environment}.env"
if [[ ! -r "$env_file" ]]; then
  echo "no env file at $env_file" >&2
  exit 1
fi

unit="reelpin-${service}-${environment}"
state="/etc/reelpin/${unit}.image"

previous=""
if [[ -r "$state" ]]; then
  previous=$(cat "$state")
fi

echo "pulling $image"
docker pull "$image"

echo "$image" > "$state"
echo "restarting $unit"
if ! systemctl restart "$unit"; then
  echo "restart failed" >&2
  if [[ -n "$previous" ]]; then
    echo "rolling back to $previous" >&2
    echo "$previous" > "$state"
    systemctl restart "$unit" || true
  fi
  exit 1
fi

# systemd reports the restart as done before the process is serving, so the
# unit is checked again after it has had a moment to fail.
sleep 5
if ! systemctl is-active --quiet "$unit"; then
  echo "$unit is not active after restart" >&2
  journalctl -u "$unit" -n 50 --no-pager >&2
  if [[ -n "$previous" ]]; then
    echo "rolling back to $previous" >&2
    echo "$previous" > "$state"
    systemctl restart "$unit" || true
  fi
  exit 1
fi

echo "$unit is running $image"
