#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(git rev-parse --show-toplevel)"

cd "$ROOT_DIR/test"
rm link.db || :

"$ROOT_DIR/bin/interceptor" go build -o foo .
"$ROOT_DIR/bin/interceptor" go build --tags A -o foo .
"$ROOT_DIR/bin/interceptor" go build --tags B -o foo .

"$ROOT_DIR/bin/executor" --link "$(go env GOTOOLDIR)/link" -- foo
"$ROOT_DIR/bin/executor" --link "$(go env GOTOOLDIR)/link" --tags A -- foo
"$ROOT_DIR/bin/executor" --link "$(go env GOTOOLDIR)/link" --tags B -- foo
