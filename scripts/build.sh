#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(git rev-parse --show-toplevel)"
cd "$ROOT_DIR"

for cmd in interceptor executor; do
	go build -v -o "$ROOT_DIR/bin/$cmd" ./cmd/$cmd
done
