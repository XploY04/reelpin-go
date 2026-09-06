# Provisioning the development host

`ssh reelpin-ec2-dev`. The Go stack runs beside the Python one and never
touches it: different ports, different units, different env file, different
Redis.

**Nothing here has been applied.** The files are in `deploy/dev/`.

## Read this before you start

The host has **911 MiB of RAM** and is already about 220 MiB into a 2 GB
swapfile with Python essentially idle. Measured idle footprints of what this
adds:

| | RSS |
|---|---|
| Go API | 35.5 MiB |
| Go worker | 30.6 MiB, before a media job |
| Redis | 8.9 MiB |
| RabbitMQ | 142.6 MiB |
| **Total** | **~218 MiB** against roughly 317 MiB free |

So it fits at idle, and has almost nothing left for a media job, whose `ffmpeg`
and `yt-dlp` are extra processes on top of the worker's own number.

RabbitMQ's default high watermark is 0.4 of *available* memory, which computes
to about 364 MiB here: more than is free. Left alone it expands into swap under
queue pressure rather than pushing back, and what the kernel kills is whichever
process is largest at the time. `deploy/dev/docker-compose.dev.yml` therefore
pins an absolute 180 MiB watermark, so a full queue refuses publishes instead,
which is visible and recoverable.

**The honest recommendation is 2 GB.** The caps below make the current host
behave predictably rather than badly, which is not the same as making it a good
place to run a media pipeline. Moving RabbitMQ to managed hosting is the other
way, and the cheaper change.

## Order

The old validation container holds port 8080, which is the port the API wants.
It is the last thing removed, not the first: it is the only Go on the host until
the replacement is running.

```sh
# 1. Reboot for the pending kernel update. 35 days of uptime, and a restart in
#    the middle of provisioning is worse than one before it.
sudo reboot

# 2. Reclaim the build image. 1.23 GB of the 1.24 GB of images is unreferenced.
sudo docker image prune -a

# 3. Redis and RabbitMQ, capped.
sudo mkdir -p /opt/reelpin-go
sudo cp deploy/dev/docker-compose.dev.yml /opt/reelpin-go/
sudo docker compose -f /opt/reelpin-go/docker-compose.dev.yml up -d
ss -tln | grep -E '6380|5672|15672'   # loopback only, all three

# 4. The environment file.
sudo cp deploy/dev/dev.env.example /etc/reelpin/dev.env
sudo chmod 0600 /etc/reelpin/dev.env
sudo chown root:root /etc/reelpin/dev.env
sudoedit /etc/reelpin/dev.env

# 5. The units. REELPIN_IMAGE is a digest, never a tag: a tag can move between
#    the dev deploy and the production promotion, which is the whole reason the
#    release workflow deploys by digest.
sudo cp deploy/dev/reelpin-go-*.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now reelpin-go-worker
# The API last: it needs 8080, and 8080 is still the old container.
sudo docker rm -f reelpin-go-pr2
sudo systemctl enable --now reelpin-go-api

# 6. Both processes, then the routing.
curl -s localhost:8080/api/v2/health/ready
curl -s localhost:8000/api/v1/health      # Python, unchanged
```

## Then nginx

`/etc/nginx/sites-available/reelpin-api` gains a `location ^~ /api/v2/` block
proxying to `127.0.0.1:8080`, above the existing `location /`. `^~` so it wins
over any regex location added later; a v2 request reaching Python 404s in a way
that reads as a missing reel rather than a routing mistake.

Test before reloading, and check Python both before and after:

```sh
sudo nginx -t && sudo systemctl reload nginx
curl -s https://api-dev.reelpin.in/api/v1/health
curl -s https://api-dev.reelpin.in/api/v2/health/ready
```

`/metrics`, the RabbitMQ management UI, Redis and the worker's metrics port are
never proxied. They are loopback-only and stay that way.

## What is already true

- The development Supabase project has all 13 Go migrations applied, 29 tables,
  and `postgis`, `vector` and `pg_trgm`. `migrate-status` shows no gaps.
- The API answers `ready: true` against that project, run locally.
- `dev.reelpin.in` is assigned to the `dev` branch in Vercel and verified.
- `REELPIN_IP_BUCKET_SECRET` is set in Vercel for preview and development. The
  same value has to go in `/etc/reelpin/dev.env`, or Go ignores the forwarded
  bucket and every web visitor shares one rate-limit bucket.

## What still needs someone

- The instance size, or a decision to move RabbitMQ off-host.
- `DEPLOY_HOST`, `DEPLOY_USER` and `DEPLOY_SSH_KEY` on the `dev` GitHub
  environment, before the release workflow can deploy anything.
- Google and Apple client credentials, if development is to exercise social
  sign-in. Everything else about them is already wired and inert.
