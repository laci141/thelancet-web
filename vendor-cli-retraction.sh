#!/usr/bin/env bash
# vendor-cli-retraction.sh — cross-compile the retraction-checker CLI to
# bin/retraction-checker-pp-cli-linux (linux/amd64).
#
# This repo ships TWO vendored binaries and the Dockerfile copies both, but
# only one had a script: vendor-cli.sh builds thelancet. The retraction-checker
# binary sat at its 2026-07-17 build with nothing recording how it was made.
# A second file rather than a flag on the existing one, deliberately: that
# script works and was repaired earlier today, and a rewrite to make it clever
# risks the thing that already runs.
#
# The CLI's logic has not changed since July — the only upstream commit touching
# it is the one that added it to main — so a rebuild brings the newer Go
# toolchain and its stdlib fixes, not new behaviour.
#
# The default source is the monorepo under ~/printing-press-library, NOT the
# Desktop copy: there are two clones on this machine on different branches.
#
# cmd/ holds two binaries, the CLI and an MCP server. Only the CLI is built.
#
# USAGE (from the bibliovera repo, Git Bash):
#   ./vendor-cli-retraction.sh
#   ./vendor-cli-retraction.sh "/c/Users/LACI/printing-press-library/library/other/retraction-checker"
set -euo pipefail
CLI_SRC="${1:-/c/Users/LACI/printing-press-library/library/other/retraction-checker}"
OUT="bin/retraction-checker-pp-cli-linux"
if [ ! -f "$CLI_SRC/go.mod" ] || [ ! -d "$CLI_SRC/cmd" ]; then
  echo "ERROR: CLI source not found at: $CLI_SRC" >&2
  exit 1
fi
echo "Vendoring from: $CLI_SRC"
( cd "$CLI_SRC" && git rev-parse --abbrev-ref HEAD && git log --oneline -1 -- . )
rm -rf cli-src-retraction && mkdir -p cli-src-retraction
# go.sum only exists when the CLI has external dependencies.
cp "$CLI_SRC/go.mod" cli-src-retraction/
[ -f "$CLI_SRC/go.sum" ] && cp "$CLI_SRC/go.sum" cli-src-retraction/ || true
cp -r "$CLI_SRC/cmd" "$CLI_SRC/internal" cli-src-retraction/
echo "Cross-compiling -> $OUT"
mkdir -p bin
( cd cli-src-retraction && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "../$OUT" ./cmd/retraction-checker-pp-cli )
# `file` is not present in every Git Bash; a missing one must not kill the
# script under set -e after a successful build.
command -v file >/dev/null && file "$OUT" || true
ls -la "$OUT"