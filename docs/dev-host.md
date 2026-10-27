# Development host

`ssh reelpin-ec2-dev` runs the public Go development API. This page records the
state applied on 2026-09-06 and how to reproduce or roll it back.

## Current layout

The host has 911 MiB RAM and 2 GiB swap. Python was stopped and disabled to
make room; its checkout, environment and unit files remain intact.

| Process | Address | Limit |
|---|---|---|
| Go API | `127.0.0.1:8080` | 192 MiB RAM, 256 MiB including swap |
| Go worker metrics | `127.0.0.1:9101` | worker: 640 MiB RAM, 1 GiB including swap |
| Redis | `127.0.0.1:6379` | existing system service, prefix `reelpin-go-dev` |
| RabbitMQ | `127.0.0.1:5672` | 256 MiB RAM, 384 MiB including swap |
| RabbitMQ management | `127.0.0.1:15672` | loopback only |

Nginx sends `api-dev.reelpin.in` to port 8080. `/api/v2` is available and the
old Python `/api/v1` is not served on this hostname while Python is stopped.

The API and worker use the separate `reelpin-go` Supabase project. The 13 Go
migrations are applied. The root-only environment is `/etc/reelpin/dev.env`;
RabbitMQ credentials are `/etc/reelpin/rabbitmq-dev.env`. Never print or copy
either into the repository.

## Capacity boundary

The current arrangement is a development compromise. It passed one complete
generic-page job and one short YouTube job, but it is not production capacity
evidence. Keep one media consumer and one light consumer. Do not add another
stateful service or another media consumer to this host.

RabbitMQ is capped at 256 MiB. Its absolute alarm is 192 MiB: 128 MiB was below
the Erlang runtime's idle reservation and blocked every publisher confirm.
Media work is mounted at `/var/lib/reelpin/work`, not tmpfs. A 500 MiB admitted
video must not consume half of system RAM.

## Provision RabbitMQ

Create `/etc/reelpin/rabbitmq-dev.env`, mode `0600`, containing
`RABBITMQ_DEFAULT_USER` and `RABBITMQ_DEFAULT_PASS`. Then:

```sh
sudo mkdir -p /opt/reelpin-go
sudo cp deploy/dev/docker-compose.dev.yml /opt/reelpin-go/
sudo docker compose -f /opt/reelpin-go/docker-compose.dev.yml up -d
sudo docker exec reelpin-rabbitmq-dev rabbitmq-diagnostics alarms
```

The last command must report no alarms. An alarm blocks publishing and leaves
jobs queued even though the API is healthy.

## Install the services

Create the image state files with the tested digest:

```sh
printf 'REELPIN_IMAGE=%s\n' \
  'ghcr.io/xploy04/reelpin-go@sha256:<digest>' \
  | sudo tee /etc/reelpin/reelpin-api-dev.image >/dev/null
sudo cp /etc/reelpin/reelpin-api-dev.image \
  /etc/reelpin/reelpin-worker-dev.image
sudo chmod 0600 /etc/reelpin/reelpin-*-dev.image
```

Install the units and disk-backed workspace:

```sh
sudo install -d -o 10001 -g 10001 -m 0700 /var/lib/reelpin/work
sudo install -m 0644 deploy/dev/reelpin-go-api.service \
  /etc/systemd/system/reelpin-api-dev.service
sudo install -m 0644 deploy/dev/reelpin-go-worker.service \
  /etc/systemd/system/reelpin-worker-dev.service
sudo systemctl daemon-reload
sudo systemctl enable --now reelpin-api-dev reelpin-worker-dev
```

Future deployments use the tested digest:

```sh
sudo deploy/reelpin-deploy.sh api dev ghcr.io/xploy04/reelpin-go@sha256:<digest>
sudo deploy/reelpin-deploy.sh worker dev ghcr.io/xploy04/reelpin-go@sha256:<digest>
```

## Nginx

The active development site contains:

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

Test before reload:

```sh
sudo nginx -t
sudo systemctl reload nginx
curl -fsS https://api-dev.reelpin.in/api/v2/health/ready
```

## Verification

The deployment is not proved by health alone. Use disposable development users
and verify:

1. Authenticated reels, jobs and library stats return 200.
2. A generic page returns 202, reaches `completed`, and its reel can be read.
3. A short YouTube video reaches `completed` with content from the requested
   video.
4. Account deletion reports both `data_deleted` and `identity_deleted` true.
5. RabbitMQ has one consumer on media, light and notifications, with no ready,
   unacknowledged or dead-letter messages after cleanup.
6. No disposable users, saves, jobs, contents or pending outbox events remain.

## Rollback to Python

The backup Nginx site is `/etc/nginx/backups/reelpin-api.python-20260906`.

```sh
sudo systemctl stop reelpin-api-dev reelpin-worker-dev
sudo cp /etc/nginx/backups/reelpin-api.python-20260906 \
  /etc/nginx/sites-enabled/reelpin-api
sudo nginx -t && sudo systemctl reload nginx
sudo systemctl enable --now reelpin-api.service reelpin-worker.service
curl -fsS https://api-dev.reelpin.in/api/v1/health/ready
```

Do not delete the Python checkout or units until this rollback path is no longer
needed.
