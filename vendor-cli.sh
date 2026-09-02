#!/usr/bin/env bash
# vendor-cli.sh — cross-compile the thelancet CLI to bin/thelancet-pp-cli-linux
# (linux/amd64), which the Docker image copies and runs (refresh + analytics).
#
# USAGE (from WEB_DIR, Git Bash), monorepo on the feat/thelancet branch:
#   ./vendor-cli.sh
#   ./vendor-cli.sh "/c/Users/LACI/printing-press-library/library/developer-tools/thelancet"
set -euo pipefail
CLI_SRC="${1:-/c/Users/LACI/printing-press-library/library/developer-tools/thelancet}"
OUT="bin/thelancet-pp-cli-linux"
if [ ! -f "$CLI_SRC/go.mod" ] || [ ! -d "$CLI_SRC/cmd" ]; then
  echo "ERROR: CLI source not found at: $CLI_SRC (check out the feat/thelancet branch)" >&2
  exit 1
fi
echo "Vendoring from: $CLI_SRC"
( cd "$CLI_SRC" && git log --oneline -1 -- . )
rm -rf cli-src && mkdir -p cli-src
cp "$CLI_SRC/go.mod" "$CLI_SRC/go.sum" cli-src/
cp -r "$CLI_SRC/cmd" "$CLI_SRC/internal" cli-src/
echo "Cross-compiling -> $OUT"
mkdir -p bin
( cd cli-src && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "../$OUT" ./cmd/thelancet-pp-cli )
command -v file >/dev/null && file "$OUT" || true
ls -la "$OUT"
