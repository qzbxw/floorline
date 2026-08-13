# syntax=docker/dockerfile:1.7

# Floorline moves real money on a marketplace that changes without notice, so
# the image is built for two things: rebuilding fast when only Go code changed,
# and carrying as little as possible into production.

FROM golang:1.26-alpine AS build

# git is needed only if a dependency resolves through it; keeping it in the
# build stage costs nothing in the final image.
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# Dependencies first, on their own layer. They change far less often than the
# source, so an ordinary code edit reuses this layer entirely instead of
# re-downloading the module graph.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# The two caches that matter. GOMODCACHE avoids re-fetching modules across
# builds, GOCACHE avoids recompiling packages that did not change — together
# they turn a cold two-minute build into a warm ten-second one.
#
# CGO is off deliberately: the SQLite driver is pure Go (modernc.org/sqlite), so
# the result is a static binary that runs on an empty image with no libc to keep
# in step with the host.
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/floorline ./cmd/floorline

# Prepare the state directory here, where there is still a shell.
#
# Docker seeds a fresh named volume from whatever is at the mount point in the
# image, ownership included. Without this the volume arrives owned by root, the
# non-root process cannot create the database, and the container dies on its
# first health check with "unable to open database file".
RUN mkdir -p /state && chown 65532:65532 /state

# Run the tests inside the build so a broken commit cannot be deployed. It is a
# separate stage rather than a step above, so `--target build` still produces a
# binary when someone needs one urgently and the suite is red.
FROM build AS test
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go vet ./... && go test ./...

FROM gcr.io/distroless/static-debian12:nonroot AS runtime

# The desk talks to Tonnel, Portals, MRKT and Gate over TLS, and formats times
# for a human, so it needs roots and zone data. Distroless static ships both,
# but they are copied explicitly so a base image change cannot silently remove
# them.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo

COPY --from=build /out/floorline /floorline
COPY --from=build --chown=65532:65532 /state /data

# Everything that must survive a redeploy lives here: the SQLite database with
# every position, signal and trade, and the Telegram session file. Both are
# pointed into the volume by default so a container started without an explicit
# DB_PATH cannot quietly write its book to a layer that is thrown away.
ENV DB_PATH=/data/floorline.db \
    TELEGRAM_SESSION=/data/tgsession.json \
    TZ=UTC
VOLUME ["/data"]

# nonroot (65532) comes from the base image. The volume has to be writable by
# it, which the compose file arranges.
USER nonroot:nonroot

# The probe checks only what the process cannot run without — that the database
# opens and reads. It deliberately does not touch the network: a marketplace
# throttling us is not an unhealthy container, and restarting on that would turn
# a slow morning into a crash loop.
HEALTHCHECK --interval=60s --timeout=10s --start-period=20s --retries=3 \
    CMD ["/floorline", "-env", "/dev/null", "health"]

ENTRYPOINT ["/floorline"]
CMD ["run"]
