# Build-only image for CI. Media tools and the worker arrive with their tasks.
FROM golang:1.26-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --no-create-home --shell /usr/sbin/nologin reelpin

COPY --from=build /out/api /usr/local/bin/reelpin-api

USER 10001:10001
EXPOSE 8000

ENTRYPOINT ["/usr/local/bin/reelpin-api"]
