#!/usr/bin/env bash
# Check that a deployed API is actually serving, from the host itself.
#
#   reelpin-smoke.sh http://127.0.0.1:8080 <image@digest>
#
# Readiness only, never a write and never an authenticated call: a smoke test
# that needs a user token is a smoke test that fails for the wrong reasons.
set -euo pipefail

base="${1:?usage: reelpin-smoke.sh <base-url> <image@digest>}"
image="${2:?missing expected image digest}"
if [[ ! "$image" =~ ^ghcr\.io/xploy04/reelpin-go@sha256:[a-f0-9]{64}$ ]]; then
  echo "smoke requires an immutable ReelPin image digest" >&2
  exit 1
fi
revision=$(docker image inspect "$image" --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}')
if [[ ! "$revision" =~ ^[a-f0-9]{40}$ ]]; then
  echo "the expected image has no valid build revision" >&2
  exit 1
fi

check_body() {
  python3 -c '
import json, sys
body = json.load(sys.stdin)
if body.get("service") != "ReelPin API" or body.get("version") != sys.argv[1]:
    sys.exit("smoke failed: wrong API identity or build version")
if body.get("ready") is not True or body.get("status") != "ok":
    sys.exit("smoke failed: API is not ready")
' "$revision"
}

for attempt in 1 2 3 4 5 6; do
  if body=$(curl -fsS --max-time 5 "${base}/api/v2/health/live"); then
    check_body <<<"$body"
    break
  fi
  if [[ "$attempt" == 6 ]]; then
    echo "liveness never answered at ${base}" >&2
    exit 1
  fi
  sleep 5
done

body=$(curl -fsS --max-time 10 "${base}/api/v2/health/ready")
check_body <<<"$body"

echo "smoke test passed"
