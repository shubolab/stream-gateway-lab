#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
mkdir -p dist
go build -trimpath -buildvcs=false -ldflags='-s -w' -o dist/stream-gateway-lab .
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags='-s -w' -o dist/stream-gateway-executor-linux-amd64 .
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -buildvcs=false -ldflags='-s -w' -o dist/stream-gateway-executor-darwin-arm64 .
gzip -n -c dist/stream-gateway-executor-linux-amd64 > dist/stream-gateway-executor-linux-amd64.gz
gzip -n -c dist/stream-gateway-executor-darwin-arm64 > dist/stream-gateway-executor-darwin-arm64.gz
cp internal/control/install.sh dist/install-executor.sh
printf '%s\n' 'Built native Gateway, Linux/amd64 and Darwin/arm64 Executors, and installer in dist/.'
