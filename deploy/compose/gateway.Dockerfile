# The gateway image: the pinned rclone, nothing else of ours (architecture 7.1).
# The container runs `rclone serve restic ...` with the command-line flags set in
# docker-compose.yml. It holds the gateway credential only, through a mounted
# rclone.conf that contains the [store] remote and no other.
#
# rclone is downloaded from downloads.rclone.org and checked against the SHA-256
# of the release archive you pass in (RCLONE_SHA256, from rclone's SHA256SUMS; see
# .env.example). A mismatch, or an empty value, fails the build.

ARG RUNTIME_IMAGE=debian:12.11-slim

FROM ${RUNTIME_IMAGE} AS tools
ARG RCLONE_VERSION=1.75.1
ARG RCLONE_SHA256
ARG RCLONE_ARCH=amd64
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl unzip \
 && rm -rf /var/lib/apt/lists/*
# rclone reads every RCLONE_<FLAG> environment variable as that flag, so the RCLONE_VERSION
# build arg (exported into this step) would be parsed as --version: unset it for the self-check.
RUN set -eu; \
    test -n "${RCLONE_SHA256}" || { echo "RCLONE_SHA256 is required" >&2; exit 1; }; \
    mkdir -p /out /tmp/dl; \
    curl -fsSL -o /tmp/dl/rclone.zip \
      "https://downloads.rclone.org/v${RCLONE_VERSION}/rclone-v${RCLONE_VERSION}-linux-${RCLONE_ARCH}.zip"; \
    echo "${RCLONE_SHA256}  /tmp/dl/rclone.zip" | sha256sum -c -; \
    unzip -j -o /tmp/dl/rclone.zip "rclone-v${RCLONE_VERSION}-linux-${RCLONE_ARCH}/rclone" -d /out; \
    chmod 0755 /out/rclone; \
    env -u RCLONE_VERSION /out/rclone version | head -1 | grep -q "rclone v${RCLONE_VERSION}"

FROM ${RUNTIME_IMAGE}
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --gid 10001 cryo \
 && useradd --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin cryo \
 && install -d -o 10001 -g 10001 -m 0750 /auth
COPY --from=tools /out/rclone /usr/local/bin/rclone
USER 10001:10001
ENTRYPOINT ["rclone"]
