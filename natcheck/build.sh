#!/bin/bash
# Собирает natcheck для macOS (Apple Silicon и Intel) и Windows в папку dist/
set -e
cd "$(dirname "$0")"
mkdir -p dist
LDFLAGS="-s -w"
GOOS=darwin  GOARCH=arm64 go build -ldflags "$LDFLAGS" -o dist/natcheck-macos-arm64 .
GOOS=darwin  GOARCH=amd64 go build -ldflags "$LDFLAGS" -o dist/natcheck-macos-intel .
GOOS=windows GOARCH=amd64 go build -ldflags "$LDFLAGS" -o dist/natcheck-windows.exe .
echo "Готово: $(pwd)/dist"
