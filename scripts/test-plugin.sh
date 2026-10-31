#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
docker run --rm -v "$PWD:/src" -v bifrost-web-providers-gomod:/go/pkg/mod \
  -v bifrost-web-providers-gocache:/root/.cache/go-build -w /src golang:1.27.3 \
  sh -c 'go build -trimpath -tags sqlite_static -buildmode=plugin -o /tmp/web-providers.so ./plugin &&
    go build -trimpath -tags sqlite_static -buildmode=plugin -o /tmp/fixture.so ./tests/pluginfixture &&
    go build -trimpath -tags sqlite_static -o /tmp/pluginhost ./tests/pluginhost &&
    /tmp/pluginhost /tmp/web-providers.so /tmp/fixture.so'
