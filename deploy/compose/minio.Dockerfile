# The LAB S3 target: MinIO server and mc, built from source. Lab only (compose profile
# "lab"); never a production target.
#
# Why this is built here: MinIO retired its community edition. The upstream repository
# (https://github.com/minio/minio) is archived and says the community edition is
# distributed as source only, with no pre-compiled binaries, and the project's container
# images can no longer be pulled. The Go module proxy still serves the last pinned
# releases as immutable pseudo-versions, verified against the checksum database
# (sum.golang.org, left at its default: never switch the checksum database off).
# So the lab builds exactly the release it was written against:
#
#   MINIO_MODULE_VERSION  v0.0.0-20250907161309-07c3a429bfed = RELEASE.2025-09-07T16-13-09Z
#   MC_MODULE_VERSION     v0.0.0-20250813083541-7394ce0dd2a8 = RELEASE.2025-08-13T08-35-41Z
#
# A pseudo-version names one commit and cannot move; there is no "latest" here. The
# binaries report a development version string (the upstream release ldflags are not
# applied). Build context is deploy/compose (see docker-compose.yml); nothing is copied
# in. A maintained lab target is an M5 decision.

ARG GO_IMAGE=golang:1.26.10-bookworm
ARG RUNTIME_IMAGE=debian:12.11-slim

# ---- build minio and mc (static, no cgo) -------------------------------------
FROM ${GO_IMAGE} AS build
ARG MINIO_MODULE_VERSION=v0.0.0-20250907161309-07c3a429bfed
ARG MC_MODULE_VERSION=v0.0.0-20250813083541-7394ce0dd2a8
ENV CGO_ENABLED=0 \
    GOFLAGS=-mod=mod \
    GOBIN=/out
RUN set -eu; \
    test -n "${MINIO_MODULE_VERSION}" || { echo "MINIO_MODULE_VERSION is required" >&2; exit 1; }; \
    test -n "${MC_MODULE_VERSION}" || { echo "MC_MODULE_VERSION is required" >&2; exit 1; }; \
    go install -trimpath -ldflags "-s -w" "github.com/minio/minio@${MINIO_MODULE_VERSION}"; \
    go install -trimpath -ldflags "-s -w" "github.com/minio/mc@${MC_MODULE_VERSION}"; \
    test -x /out/minio; \
    test -x /out/mc

# ---- the image ---------------------------------------------------------------
FROM ${RUNTIME_IMAGE}
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --gid 10002 minio \
 && useradd --uid 10002 --gid 10002 --create-home --shell /usr/sbin/nologin minio \
 && install -d -o 10002 -g 10002 -m 0750 /data
COPY --from=build /out/minio /out/mc /usr/local/bin/
USER 10002:10002
VOLUME /data
EXPOSE 9000 9001
ENTRYPOINT ["/usr/local/bin/minio"]
CMD ["server", "/data"]
