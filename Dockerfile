# Build-only image for CI. Media tools and the worker arrive with their tasks.
FROM golang:1.26-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/maintenance ./cmd/maintenance

FROM debian:bookworm-slim

# ffmpeg is what turns a downloaded video into the small audio track a
# transcript needs. yt-dlp is pinned and checksum-verified in Task 22; this
# stage installs the interpreter it needs.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates ffmpeg python3 \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --no-create-home --shell /usr/sbin/nologin reelpin

COPY --from=build /out/api /usr/local/bin/reelpin-api
COPY --from=build /out/worker /usr/local/bin/reelpin-worker
COPY --from=build /out/maintenance /usr/local/bin/reelpin-maintenance

USER 10001:10001
EXPOSE 8000

ENTRYPOINT ["/usr/local/bin/reelpin-api"]
