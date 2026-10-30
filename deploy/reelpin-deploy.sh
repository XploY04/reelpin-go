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

case "$environment" in
  dev|production) ;;
  *) echo "unknown environment $environment" >&2; exit 1 ;;
esac

if [[ ! "$image" =~ ^ghcr\.io/xploy04/reelpin-go@sha256:[a-f0-9]{64}$ ]]; then
  echo "refusing to deploy $image: pass an immutable ReelPin image digest" >&2
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
  previous=$(sed -n 's/^REELPIN_IMAGE=//p' "$state")
fi

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

verify_service() {
  sleep 5
  if ! systemctl is-active --quiet "$unit"; then
    echo "$unit is not active after restart" >&2
    journalctl -u "$unit" -n 50 --no-pager >&2
    return 1
  fi
  if [[ "$service" == api ]]; then
    bash "$script_dir/reelpin-smoke.sh" http://127.0.0.1:8080 "$1"
  fi
}

rollback() {
  if [[ -z "$previous" ]]; then
    echo "no previous image available for rollback" >&2
    return 1
  fi
  echo "rolling back to $previous" >&2
  if ! printf 'REELPIN_IMAGE=%s\n' "$previous" > "$state"; then
    return 1
  fi
  if ! systemctl restart "$unit"; then
    return 1
  fi
  if ! verify_service "$previous"; then
    return 1
  fi
  echo "rollback verified: $unit is running $previous" >&2
}

echo "pulling $image"
docker pull "$image"

printf 'REELPIN_IMAGE=%s\n' "$image" > "$state"
echo "restarting $unit"
if ! systemctl restart "$unit"; then
  echo "restart failed" >&2
  if ! rollback; then
    echo "rollback failed" >&2
  fi
  exit 1
fi

if ! verify_service "$image"; then
  if ! rollback; then
    echo "rollback failed" >&2
  fi
  exit 1
fi

echo "$unit is running $image"
