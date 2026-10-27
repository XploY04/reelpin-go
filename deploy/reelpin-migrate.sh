#!/usr/bin/env bash
set -euo pipefail

environment="${1:?usage: reelpin-migrate.sh <dev|production> <image@digest> <env-file>}"
image="${2:?missing image digest}"
env_file="${3:?missing env file}"

case "$environment" in
  dev|production) ;;
  *) echo "unknown deployment environment" >&2; exit 1 ;;
esac
if [[ ! "$image" =~ ^ghcr\.io/xploy04/reelpin-go@sha256:[a-f0-9]{64}$ ]]; then
  echo "migration requires an immutable ReelPin image digest" >&2
  exit 1
fi

idle=()
if [[ "$environment" = production ]]; then
  for service in api worker; do
    unit="reelpin-${service}-production"
    loaded=$(systemctl list-units --all --no-legend --no-pager "$unit.service")
    installed_status=0
    installed=$(systemctl list-unit-files --no-legend --no-pager "$unit.service" 2>&1) || installed_status=$?
    # Ubuntu systemd returns 1 with no output when no unit file matches.
    if [[ "$installed_status" -ne 0 && ( "$installed_status" -ne 1 || -n "$installed" ) ]]; then
      printf 'migration blocked: inventorying %s failed (exit %s)\n%s\n' \
        "$unit" "$installed_status" "$installed" >&2
      exit "$installed_status"
    fi
    if [[ -n "$loaded" || -n "$installed" ]]; then
      state=$(systemctl show "$unit" --property=ActiveState --value)
      if [[ "$state" != inactive ]]; then
        echo "migration blocked: $unit must be drained and stopped first" >&2
        exit 1
      fi
    fi
    running=$(docker ps --filter "name=^/${unit}$" --format '{{.Names}}')
    if [[ -n "$running" ]]; then
      echo "migration blocked: $unit container is still running" >&2
      exit 1
    fi
  done

  vhost=$(python3 -c '
import sys
from urllib.parse import urlparse, unquote
values = dict(line.rstrip("\n").split("=", 1) for line in open(sys.argv[1])
              if "=" in line and not line.lstrip().startswith("#"))
if values.get("ENVIRONMENT") != "production":
    sys.exit("migration blocked: expected production configuration")
url = urlparse(values.get("RABBITMQ_URL", ""))
if url.scheme != "amqp" or not url.hostname:
    sys.exit("migration blocked: invalid RabbitMQ configuration")
if url.hostname not in ("localhost", "127.0.0.1", "::1") or url.port not in (None, 5672):
    sys.exit("migration blocked: production broker must be local")
print(unquote(url.path[1:]) or "/")
' "$env_file")
  queues=$(docker exec --env 'RABBITMQ_CTL_ERL_ARGS=+S 1:1 +A 1' reelpin-rabbitmq-production \
    rabbitmqctl list_queues --quiet --timeout 10 --formatter=json --vhost "$vhost" \
    name messages_ready messages_unacknowledged consumers)
  broker_state=$(python3 -c '
import json, sys
queues = json.load(sys.stdin)
required = {"reelpin.processing.media", "reelpin.processing.light", "reelpin.notifications", "reelpin.scheduled"}
seen = set()
if not isinstance(queues, list):
    sys.exit("migration blocked: invalid broker report")
if not queues:
    print("bootstrap")
    sys.exit(0)
for row in queues:
    name = row.get("name")
    counts = [row.get(key) for key in ("messages_ready", "messages_unacknowledged", "consumers")]
    if not isinstance(name, str) or any(type(value) is not int or value < 0 for value in counts):
        sys.exit("migration blocked: incomplete broker report")
    seen.add(name)
    ready, unacked, consumers = counts
    deferred = name == "reelpin.scheduled.delay" or name.endswith(".dead")
    if unacked or consumers or (ready and not deferred):
        sys.exit("migration blocked: broker work has not drained")
if not required.issubset(seen):
    sys.exit("migration blocked: work queues are missing")
print("drained")
' <<<"$queues")
  idle=("--require-idle=$broker_state")
fi

docker run --rm --no-healthcheck --network host --memory 128m --memory-swap 128m --cpus 0.25 \
  --env-file "$env_file" --entrypoint /usr/local/bin/reelpin-maintenance \
  "$image" migrate "${idle[@]}"
