#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(git rev-parse --show-toplevel)"

cd "$ROOT_DIR/test"
dbpath=$(mktemp --tmpdir golinkinterceptor.db.XXXXXXXXXX)
trap 'rm -f "$dbpath"' EXIT

"$ROOT_DIR/bin/interceptor" --db "$dbpath" -- go build -o foo .
"$ROOT_DIR/bin/interceptor" --db "$dbpath" -- go build --tags A -o foo .
"$ROOT_DIR/bin/interceptor" --db "$dbpath" -- go build --tags B -o foo .

"$ROOT_DIR/bin/executor" --db "$dbpath" --link "$(go env GOTOOLDIR)/link" -- foo
"$ROOT_DIR/bin/executor" --db "$dbpath" --link "$(go env GOTOOLDIR)/link" --tags A -- foo
"$ROOT_DIR/bin/executor" --db "$dbpath" --link "$(go env GOTOOLDIR)/link" --tags B -- foo
