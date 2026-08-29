#!/usr/bin/env bash
set -euo pipefail
VERSION="${1:-dev}"
: "${GOOS:=linux}"
export CGO_ENABLED=1
case "$GOOS" in windows) ext=.dll;; darwin) ext=.dylib;; *) ext=.so;; esac
go build -buildmode=c-shared -trimpath -buildvcs=false -ldflags="-s -w -buildid= -X main.version=$VERSION" -o "cpa-usage-manager${ext}" .
