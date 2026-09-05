#!/usr/bin/env bash
# Check that a deployed API is actually serving, from the host itself.
#
#   reelpin-smoke.sh http://127.0.0.1:8000
#
# Readiness only, never a write and never an authenticated call: a smoke test
# that needs a user token is a smoke test that fails for the wrong reasons.
set -euo pipefail

base="${1:?usage: reelpin-smoke.sh <base-url>}"

for attempt in 1 2 3 4 5 6; do
  if curl -fsS --max-time 5 "${base}/api/v1/health/live" >/dev/null; then
    break
  fi
  if [[ "$attempt" == 6 ]]; then
    echo "liveness never answered at ${base}" >&2
    exit 1
  fi
  sleep 5
done

body=$(curl -fsS --max-time 10 "${base}/api/v1/health/ready")
echo "$body"

# jq is not assumed: the field is unambiguous enough to grep.
if ! grep -q '"ready":true' <<<"$body"; then
  echo "the service is live but not ready" >&2
  exit 1
fi

echo "smoke test passed"
