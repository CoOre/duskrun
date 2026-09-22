# Multi-stage build for the single static duskrun binary with an embedded SPA.
#
# Stage 1 builds the React/Vite frontend into internal/web/dist. Stage 2 compiles
# the Go binary, overlaying that built dist so `go:embed` picks up the real bundle.
# dist/ is generated and gitignored; outside Docker, a build without `make ui`
# embeds internal/web/placeholder.html instead and says so on every page.
# The runtime needs the external dump clients (pg_dump, mysqldump, mongodump,
# redis-cli) since duskrun streams from them; age encryption is pure-Go and needs
# no binary. We therefore ship a slim Debian runtime with the DB clients rather
# than scratch/distroless.

# The web and Go stages run on the build host's native platform ($BUILDPLATFORM)
# and cross-compile: the SPA is platform-independent and Go needs only GOARCH.
# Only the runtime stage runs per target platform, so a multi-arch build does
# not compile Go under QEMU emulation.
FROM --platform=$BUILDPLATFORM node:26-bookworm-slim AS webbuild
WORKDIR /app
# Install deps against the lockfile first for layer caching.
COPY web/package.json web/package-lock.json ./web/
RUN cd web && npm ci
COPY web ./web
# Vite writes to ../internal/web/dist → /app/internal/web/dist.
RUN cd web && npm run build

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Overlay the freshly built SPA so the binary embeds the real UI, not the placeholder.
COPY --from=webbuild /app/internal/web/dist ./internal/web/dist
# .dockerignore excludes .git, so Go cannot stamp the revision itself here — the
# version has to be passed in. `make docker` and docker-compose both do. Left
# unset the image reports "unknown", which is at least honest; the previous
# behaviour was to report a hardcoded 0.0.0-dev forever.
ARG VERSION=unknown
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/duskrun ./cmd/duskrun

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gnupg \
    && install -d /usr/share/postgresql-common/pgdg \
    && curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc \
        | gpg --dearmor -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg \
    && echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg] https://apt.postgresql.org/pub/repos/apt bookworm-pgdg main" \
        > /etc/apt/sources.list.d/pgdg.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        postgresql-client-14 \
        postgresql-client-15 \
        postgresql-client-16 \
        postgresql-client-17 \
        postgresql-client-18 \
        default-mysql-client \
        redis-tools \
    && rm -rf /var/lib/apt/lists/*

# mongodump ships outside Debian, and MongoDB's apt repo carries the tools for
# amd64 only — on arm64 `mongodb-database-tools` simply does not exist there. The
# official tarball does cover both, so it is used for every architecture rather
# than having the image work on one and silently lack mongodump on the other.
# The arm64 build is the Ubuntu 22.04 one (glibc 2.35), which runs on bookworm's
# 2.36; MongoDB publishes no Debian arm64 build.
ARG MONGO_TOOLS_VERSION=100.14.0
RUN set -eu; \
    case "$(dpkg --print-architecture)" in \
        amd64) tools="mongodb-database-tools-debian12-x86_64-${MONGO_TOOLS_VERSION}" ;; \
        arm64) tools="mongodb-database-tools-ubuntu2204-arm64-${MONGO_TOOLS_VERSION}" ;; \
        *) echo "no mongodb-database-tools build for $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://fastdl.mongodb.org/tools/db/${tools}.tgz" -o /tmp/mongotools.tgz; \
    tar -xzf /tmp/mongotools.tgz -C /tmp; \
    install -m 0755 "/tmp/${tools}/bin/mongodump" /usr/local/bin/mongodump; \
    install -m 0755 "/tmp/${tools}/bin/mongorestore" /usr/local/bin/mongorestore; \
    rm -rf /tmp/mongotools.tgz "/tmp/${tools}"; \
    mongodump --version | head -1
RUN useradd --system --create-home --home-dir /var/lib/duskrun duskrun
COPY --from=build /out/duskrun /usr/local/bin/duskrun
USER duskrun
WORKDIR /var/lib/duskrun
EXPOSE 8080
ENTRYPOINT ["duskrun"]
CMD ["serve"]
