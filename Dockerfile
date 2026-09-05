# One image, three binaries. Base images are pinned by digest: a tag can be
# moved, a digest cannot, so a rebuild months from now produces the same layers.
FROM golang:1.26-bookworm@sha256:9fdc884aacc3bec89b20ffc69f4bb369c78210e3e4f600387b5128b12c199f81 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# -trimpath keeps build paths out of the binary. The running version comes from
# APP_VERSION at runtime, so the same image can be labelled without a rebuild.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/maintenance ./cmd/maintenance

# yt-dlp is fetched in the build stage and verified against a pinned checksum.
# A download that does not match is a supply-chain event, and the build stops.
FROM build AS ytdlp
ARG YTDLP_VERSION=2026.08.19
ARG YTDLP_SHA256=58162f9bfdc27458ea47bfcb311cf47028f17d8154a8bf7d689861d46399230a
ADD https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/yt-dlp_linux /out/yt-dlp
RUN echo "${YTDLP_SHA256}  /out/yt-dlp" | sha256sum -c - && chmod 0755 /out/yt-dlp

FROM debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171

# ffmpeg turns a downloaded video into the small audio track a transcript needs.
# No compiler and no package manager cache stay in the image.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates ffmpeg \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --no-create-home --shell /usr/sbin/nologin reelpin

COPY --from=build /out/api /usr/local/bin/reelpin-api
COPY --from=build /out/worker /usr/local/bin/reelpin-worker
COPY --from=build /out/maintenance /usr/local/bin/reelpin-maintenance
COPY --from=ytdlp /out/yt-dlp /usr/local/bin/yt-dlp

# The root filesystem is meant to be mounted read-only. This is the one place
# the worker writes, and it should be a bounded tmpfs or volume.
ENV WORKER_TEMP_ROOT=/var/tmp/reelpin-runs
RUN mkdir -p /var/tmp/reelpin-runs && chown 10001:10001 /var/tmp/reelpin-runs
VOLUME ["/var/tmp/reelpin-runs"]

ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.source="https://github.com/XploY04/reelpin-go" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="UNLICENSED"

USER 10001:10001
EXPOSE 8000 9100

# Liveness never touches the database, so a failing check means the process
# itself is gone, not that a dependency is slow.
#
# The API and the worker serve different endpoints on different ports, and one
# image runs both, so the URL comes from an environment variable the deployment
# sets. The default matches the API's default port; a worker deployment sets
# HEALTHCHECK_URL to its own metrics listener.
ENV HEALTHCHECK_URL=http://127.0.0.1:8000/api/v1/health/live
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD /usr/local/bin/reelpin-maintenance healthcheck --url "$HEALTHCHECK_URL"

ENTRYPOINT ["/usr/local/bin/reelpin-api"]
