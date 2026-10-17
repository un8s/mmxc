# --- Build stage -------------------------------------------------------------
# ncruces/go-sqlite3 runs SQLite as Wasm (no cgo), so we build a fully static binary
# (CGO_ENABLED=0) and never need a C toolchain. The embedded
# internal/mmxc/assets/version.txt and internal/mmxc/assets/history.txt are
# committed, so the build does not need git or the
# `just gen` step.
FROM golang:1.26-alpine AS build

# TARGETOS/TARGETARCH are provided automatically by BuildKit/buildx and let a
# single Dockerfile cross-compile for the NAS arch (e.g. amd64 or arm64).
ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src

# Cache module downloads independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# -buildvcs=false: the build context excludes .git (see .dockerignore).
# -s -w: strip debug info to shrink the binary.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/mmxc ./cmd/mmxc

# --- Runtime stage -----------------------------------------------------------
FROM alpine:3.20

# OCI metadata. No image is published (see README), but the source label would
# link one pushed to ghcr.io to its GitHub repository.
LABEL org.opencontainers.image.source="https://github.com/un8s/mmxc" \
      org.opencontainers.image.description="Self-hosted web app for Money Manager Ex databases" \
      org.opencontainers.image.licenses="MIT"

# ca-certificates: outbound HTTPS (OIDC, Ollama).
# tzdata: correct local-time rendering of dates.
# poppler-utils: reliable PDF statement text extraction (pdftotext).
RUN apk add --no-cache ca-certificates poppler-utils tzdata wget \
    && adduser -D -H -u 1000 mmxc

WORKDIR /app
COPY --from=build /out/mmxc /usr/local/bin/mmxc

# Runtime data (db, backups, statements) lives here; mount a volume over it.
RUN mkdir -p /app/data && chown -R mmxc:mmxc /app

ENV MMXC_CONFIG=/app/mmxc.yaml
EXPOSE 8080

# Default-mode (no auth) healthcheck: any 2xx on / means the server is up.
# If you enable OIDC auth, the root path redirects off-host; drop or relax this.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/ || exit 1

USER mmxc
ENTRYPOINT ["/usr/local/bin/mmxc"]
