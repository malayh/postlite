# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Build stage. go-sqlite3 uses cgo, so we need a C toolchain (present in the
# full golang image, not the alpine one), and postlite requires the `vtable`
# build tag — without it the package does not compile.
# ---------------------------------------------------------------------------
FROM golang:1.22-bookworm AS build

WORKDIR /src

# Download modules first so they are cached independently of the source.
COPY go.mod go.sum ./
RUN go mod download

# Build the server.
COPY . .
ENV CGO_ENABLED=1
RUN go build -tags vtable -ldflags "-s -w" -o /out/postlite ./cmd/postlite

# ---------------------------------------------------------------------------
# Runtime stage. The cgo binary is dynamically linked against glibc, so we run
# it on a slim Debian base that provides a matching libc.
#
# The image runs as root so that a bind-mounted database file (and the WAL /
# journal files SQLite creates next to it) are writable regardless of host
# ownership. To harden, run with `--user "$(id -u):$(id -g)"` and make sure the
# mounted file is writable by that user.
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /data

COPY --from=build /out/postlite /usr/local/bin/postlite

WORKDIR /data
EXPOSE 5432

# All configuration is read from the environment (flags still override). See the
# CLI flags in cmd/postlite/main.go.
#   POSTLITE_DATA_DIR  directory of .db files (database name selects the file)
#   POSTLITE_DATABASE  a single .db file served to every connection (overrides DATA_DIR)
#   POSTLITE_USER      username clients authenticate as            (default: postgres)
#   POSTLITE_PASSWORD  password clients must supply; empty = no auth
#   POSTLITE_ADDR      bind address                                (default: :5432)
ENV POSTLITE_DATA_DIR=/data

ENTRYPOINT ["postlite"]
