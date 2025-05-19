#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(git rev-parse --show-toplevel)"
LOG_LEVEL=${LOG_LEVEL:-0}

cd "$ROOT_DIR/test"
dbpath=$(mktemp --tmpdir golinkinterceptor.db.XXXXXXXXXX)
trap 'rm -f "$dbpath"' EXIT

"$ROOT_DIR/bin/interceptor" --log-level "$LOG_LEVEL" --db "$dbpath" -- go build -o foo .
"$ROOT_DIR/bin/interceptor" --log-level "$LOG_LEVEL" --db "$dbpath" -- go build --tags A -o foo .
"$ROOT_DIR/bin/interceptor" --log-level "$LOG_LEVEL" --db "$dbpath" -- go build --tags B -o foo .

"$ROOT_DIR/bin/executor" --log-level "$LOG_LEVEL" --db "$dbpath" --link "$(go env GOTOOLDIR)/link" -- foo
"$ROOT_DIR/bin/executor" --log-level "$LOG_LEVEL" --db "$dbpath" --link "$(go env GOTOOLDIR)/link" --tags A -- foo
"$ROOT_DIR/bin/executor" --log-level "$LOG_LEVEL" --db "$dbpath" --link "$(go env GOTOOLDIR)/link" --tags B -- foo
