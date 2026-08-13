#!/usr/bin/env bash
#
# Build here, run there.
#
# The desk lives on a 2 GB box that also hosts a Postgres and a Redis. Compiling
# Go on it does not merely take a while — it takes the machine down: a single
# `compile` process holding half a gigabyte pushes the host into swap, the build
# stops making progress, and the neighbouring services start timing out. The
# Dockerfile now caps its own parallelism, which stops the thrashing, but the
# honest answer on a host that size is not to compile there at all.
#
# So: build and test on the workstation, ship the finished image over ssh, and
# switch the container only once the new one reports healthy. Roughly ten
# megabytes travel; nothing is compiled remotely.
#
# Usage: deploy/ship.sh [user@host] [remote-dir]
set -euo pipefail

HOST="${1:-root@144.172.105.52}"
DIR="${2:-/root/floorline}"
IMAGE=floorline:latest
ROLLBACK=floorline:rollback
# The server's architecture, not the workstation's. An arm64 laptop building for
# an amd64 host without this produces an image that loads fine and then fails to
# execute, which surfaces as a container restart loop rather than as a build
# error.
PLATFORM="${PLATFORM:-linux/amd64}"

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
ssh_do() { ssh -o BatchMode=yes -o ConnectTimeout=15 "$HOST" "$@"; }

say "Building $IMAGE for $PLATFORM (the suite runs inside the image)"
# --provenance/--sbom off: the attestation manifests they add turn a single
# image into a manifest list, which `docker load` on the far side refuses.
DOCKER_BUILDKIT=1 docker build \
  --platform "$PLATFORM" \
  --target runtime \
  --provenance=false --sbom=false \
  --build-arg "VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" \
  -t "$IMAGE" .

LOCAL_ID=$(docker image inspect "$IMAGE" --format '{{.Id}}')
say "Built $LOCAL_ID"

say "Pinning the rollback target"
# What gets pinned is the image the container is *actually running*, read off the
# container itself, rather than whatever the `latest` tag currently points at.
#
# Those are not the same thing and the difference is the whole value of the pin.
# `latest` moves the moment anything is loaded or built on the host, so tagging
# it can just as easily pin the image being replaced as the one already replaced
# — and the first run of this script did exactly that: the tag step failed, the
# `|| true` swallowed it, and the rollback tag was left pointing two versions
# back at an image from eight hours earlier. A rollback that silently lands on
# the wrong build is worse than no rollback, because it will be trusted.
RUNNING=$(ssh_do "docker inspect -f '{{.Image}}' floorline" 2>/dev/null || true)
if [ -n "$RUNNING" ]; then
  # No `|| true` here. If there is something to roll back to and we cannot pin
  # it, the deploy stops: proceeding would remove the only way back.
  ssh_do "docker tag $RUNNING $ROLLBACK"
  echo "   rollback pinned to the running image $RUNNING"
else
  echo "   nothing is running — first deploy, no rollback target"
fi

say "Shipping to $HOST"
docker save "$IMAGE" | gzip -1 | ssh -o BatchMode=yes -o ConnectTimeout=15 "$HOST" 'docker load'

REMOTE_ID=$(ssh_do "docker image inspect $IMAGE --format '{{.Id}}'")
if [ "$REMOTE_ID" != "$LOCAL_ID" ]; then
  echo "the image on the far side is $REMOTE_ID, not the $LOCAL_ID just built" >&2
  exit 1
fi

say "Switching the container over"
# No --build. The image is already there, and a build here is the thing this
# script exists to avoid.
ssh_do "cd $DIR && docker compose up -d"

say "Waiting for it to report healthy"
for _ in $(seq 1 60); do
  state=$(ssh_do "docker inspect -f '{{.State.Health.Status}}' floorline" 2>/dev/null || echo unknown)
  case "$state" in
    healthy)
      say "Healthy on $REMOTE_ID"
      ssh_do "docker logs floorline --since 2m 2>&1 | tail -20"
      exit 0
      ;;
    unhealthy)
      break
      ;;
  esac
  sleep 5
done

# A desk that will not come up is worse than an old desk that works, and the
# rollback has to happen without waiting for someone to read this output.
say "It did not come up — rolling back"
ssh_do "docker logs floorline --since 5m 2>&1 | tail -40" || true
if [ -z "$RUNNING" ]; then
  echo "nothing to roll back to: this was the first deploy. The container is left as it is." >&2
  exit 1
fi
ssh_do "docker tag $ROLLBACK $IMAGE && cd $DIR && docker compose up -d"
echo "rolled back to $RUNNING; the new image is still on the host, untagged" >&2
exit 1
