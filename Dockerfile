# syntax=docker/dockerfile:1

# ---- Build ----
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
# tidy fills in any go.sum entries that are missing, then build a static binary.
ARG VERSION=dev
RUN go mod tidy && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/blockparty ./cmd/blockparty

# ---- rclone, for off-site backups ----
# The official release, checked against its published SHA-256.
FROM debian:bookworm-slim AS rclone
ARG TARGETARCH
ARG RCLONE_VERSION=1.75.2
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl unzip && \
    case "${TARGETARCH:-amd64}" in \
      amd64) sum=349ac8fba6ff65d6247043f1750cdcb518ec5d500ef91463a10d37c0ccdf3702 ;; \
      arm64) sum=7e1e8d69654941b7b7df84ee74c5f7fb09cce7d5496947fb7bf16b55ff427d10 ;; \
      *) echo "no rclone for ${TARGETARCH}" && exit 1 ;; \
    esac && \
    curl -fsSL -o /tmp/rclone.zip "https://github.com/rclone/rclone/releases/download/v${RCLONE_VERSION}/rclone-v${RCLONE_VERSION}-linux-${TARGETARCH:-amd64}.zip" && \
    echo "${sum}  /tmp/rclone.zip" | sha256sum -c - && \
    unzip -j /tmp/rclone.zip "*/rclone" -d /out && chmod 755 /out/rclone

# ---- Run ----
# Debian rather than Alpine: Mojang's Bedrock server needs the standard GNU C
# library and libcurl, which Alpine doesn't have.
FROM debian:bookworm-slim
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates tzdata libcurl4 libcap2-bin && \
    rm -rf /var/lib/apt/lists/* && \
    mkdir -p /config /data && chown 99:100 /config /data
COPY --from=build /out/blockparty /usr/local/bin/blockparty
COPY --from=rclone /out/rclone /usr/local/bin/rclone
# Lets the panel use port 53 (DNS) without running as root.
RUN setcap cap_net_bind_service=+ep /usr/local/bin/blockparty
# The panel runs as nobody:users (99:100, the owner Unraid uses for appdata),
# or PUID and PGID. It starts as root only to give a brand-new, root-owned
# data folder to that user, then switches to it (see cmd/blockparty/user_unix.go).
ENV HOME=/data
VOLUME ["/config", "/data"]
# 8443/tcp = web panel
# 53 = built-in DNS
# 19132/udp = console server list (RakNet); 19132/tcp = NetherNet join
# 19134-19199 = game servers the panel runs (one pair of ports each)
# 49152-50999/udp = Bedrock NetherNet gameplay for those servers
# 19300-19399/udp = the list's own NetherNet gameplay, only used when NetherNet is on
EXPOSE 8443/tcp 53/udp 53/tcp 19132/udp 19132/tcp 19134-19199/udp 19134-19199/tcp 49152-50999/udp 19300-19399/udp
# Game servers get time to save their worlds when the container stops; give
# the container at least STOP_TIMEOUT (30s) plus a little with --stop-timeout.
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/blockparty"]
