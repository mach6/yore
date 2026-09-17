#!/usr/bin/env bash
#
# smoke.sh; checks that a built server image starts and can write its database.
#
# Runs the image the way a deployment does (its own entrypoint, a fresh
# anonymous /data volume, a token from the environment) and requires
# GET /v1/ready to answer 200. /v1/ready commits a probe transaction, so it
# fails when the server cannot write /data, not just when it is down.
#
# Usage: docker/smoke.sh <image>
set -euo pipefail

image="${1:?usage: smoke.sh <image>}"

docker run --rm --entrypoint /yore "$image" version

cid=$(docker run -d -e YORE_TOKEN=smoke-test -p 127.0.0.1::8080 "$image")
cleanup() { docker rm -f -v "$cid" >/dev/null; }
trap cleanup EXIT

fail() {
  echo "smoke: $1; the server log follows" >&2
  docker logs "$cid" >&2
  exit 1
}

# An empty answer means the container has already exited.
port=$(docker port "$cid" 8080/tcp 2>/dev/null | head -n1) || true
[[ -n "$port" ]] || fail "the server exited at startup"

curl --silent --show-error --fail --retry 10 --retry-all-errors \
  --retry-delay 1 --max-time 5 "http://127.0.0.1:${port##*:}/v1/ready" ||
  fail "the server never became ready"
echo
