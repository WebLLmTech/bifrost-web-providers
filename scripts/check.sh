#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
docker run --rm -v "$PWD:/src" -v bifrost-web-providers-gomod:/go/pkg/mod \
  -v bifrost-web-providers-gocache:/root/.cache/go-build -w /src golang:1.27.3 \
  sh -c 'go test -race -coverprofile=coverage.out ./... && go vet ./...'
