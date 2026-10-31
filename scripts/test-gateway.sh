#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
image="${BIFROST_IMAGE:-bifrost-web-providers:review}"
docker build -f tests/gateway/Dockerfile --build-arg "BIFROST_IMAGE=$image" -t bifrost-web-providers:gateway-test .
docker run --rm --network none -e EXPECTED_BIFROST_VERSION="${EXPECTED_BIFROST_VERSION:-}" bifrost-web-providers:gateway-test
