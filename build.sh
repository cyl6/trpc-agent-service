#!/usr/bin/env bash
# 构建项目：产出 bin/trpc-service。
set -euo pipefail
cd "$(dirname "$0")"

VERSION="${VERSION:-$(git rev-parse --short HEAD 2>/dev/null || echo dev)}"
LDFLAGS="-X github.com/cyl6/trpc-agent-service/trpcservice.Version=$(cat VERSION 2>/dev/null || echo 0.1.0)"
LDFLAGS="${LDFLAGS} -X github.com/cyl6/trpc-agent-service/trpcservice.GitCommit=${VERSION}"

mkdir -p bin
CGO_ENABLED=0 go build -trimpath -ldflags="${LDFLAGS}" -o bin/trpc-service ./cmd/trpc-service
echo "built bin/trpc-service"
