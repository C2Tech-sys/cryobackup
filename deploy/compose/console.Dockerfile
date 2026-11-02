# The console image: cryo-console (serve and maintain) plus the pinned restic and
# rclone that `maintain` runs through restic's rclone: backend (architecture 7.1,
# 7.3). One image, two services: `console` runs `serve`, `maintenance` runs
# `maintain --loop`. The serve process never starts restic or rclone and holds no
# storage credential; the binaries are present because the image is shared.
#
# Build context is the repository root (see docker-compose.yml). restic and rclone
# are downloaded from their vendors' release pages and checked against the SHA-256
# of the archive you pass in; a mismatch, or an empty value, fails the build.
# Get the values from the vendors' SHA256SUMS files (see .env.example).

ARG GO_IMAGE=golang:1.26.10-bookworm
ARG RUNTIME_IMAGE=debian:12.11-slim

# ---- build cryo-console (static, no cgo) -------------------------------------
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w \
        -X github.com/c2tech-sys/cryobackup/internal/buildinfo.Version=${VERSION} \
        -X github.com/c2tech-sys/cryobackup/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/c2tech-sys/cryobackup/internal/buildinfo.Date=${DATE}" \
      -o /out/cryo-console ./cmd/cryo-console

# ---- fetch and verify restic and rclone --------------------------------------
FROM ${RUNTIME_IMAGE} AS tools
ARG RESTIC_VERSION=0.19.1
ARG RESTIC_SHA256
ARG RESTIC_ARCH=amd64
ARG RCLONE_VERSION=1.75.1
ARG RCLONE_SHA256
ARG RCLONE_ARCH=amd64
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl bzip2 unzip \
 && rm -rf /var/lib/apt/lists/*
# rclone reads every RCLONE_<FLAG> environment variable as that flag, so the RCLONE_VERSION
# build arg (exported into this step) would be parsed as --version: unset it for the self-check.
RUN set -eu; \
    test -n "${RESTIC_SHA256}" || { echo "RESTIC_SHA256 is required" >&2; exit 1; }; \
    test -n "${RCLONE_SHA256}" || { echo "RCLONE_SHA256 is required" >&2; exit 1; }; \
    mkdir -p /out /tmp/dl; \
    curl -fsSL -o /tmp/dl/restic.bz2 \
      "https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/restic_${RESTIC_VERSION}_linux_${RESTIC_ARCH}.bz2"; \
    echo "${RESTIC_SHA256}  /tmp/dl/restic.bz2" | sha256sum -c -; \
    bunzip2 -c /tmp/dl/restic.bz2 > /out/restic; \
    chmod 0755 /out/restic; \
    sha256sum /out/restic | cut -d' ' -f1 > /out/restic.sha256; \
    curl -fsSL -o /tmp/dl/rclone.zip \
      "https://downloads.rclone.org/v${RCLONE_VERSION}/rclone-v${RCLONE_VERSION}-linux-${RCLONE_ARCH}.zip"; \
    echo "${RCLONE_SHA256}  /tmp/dl/rclone.zip" | sha256sum -c -; \
    unzip -j -o /tmp/dl/rclone.zip "rclone-v${RCLONE_VERSION}-linux-${RCLONE_ARCH}/rclone" -d /out; \
    chmod 0755 /out/rclone; \
    /out/restic version | grep -q "restic ${RESTIC_VERSION} " ; \
    env -u RCLONE_VERSION /out/rclone version | head -1 | grep -q "rclone v${RCLONE_VERSION}"

# ---- the image ---------------------------------------------------------------
FROM ${RUNTIME_IMAGE}
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --gid 10001 cryo \
 && useradd --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin cryo \
 && install -d -o 10001 -g 10001 -m 0750 /data /artifacts /auth /run/cryo
COPY --from=build /out/cryo-console /usr/local/bin/cryo-console
COPY --from=tools /out/restic /out/rclone /usr/local/bin/
COPY --from=tools /out/restic.sha256 /usr/local/share/cryobackup/restic.sha256
USER 10001:10001
ENTRYPOINT ["cryo-console"]
CMD ["serve", "--config", "/config/config.json"]
