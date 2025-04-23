#!/usr/bin/env bash
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

cd "$DIR/interceptor"
go build -o interceptor

cd "$DIR/executor"
go build -o executor

cd "$DIR/tests"
rm link.db ||:
../interceptor/interceptor go build --tags A -o foo .
../interceptor/interceptor go build --tags B -o foo .

../executor/executor --link /home/lenaic/.local/go/pkg/tool/linux_amd64/link --tags A -- foo
../executor/executor --link /home/lenaic/.local/go/pkg/tool/linux_amd64/link --tags B -- foo
