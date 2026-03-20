#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

GOFLAGS="${GOFLAGS:+$GOFLAGS }-buildvcs=false" go install ./cmd/lit

# `lnks` remains as a compatibility entrypoint for local projects still referencing it.
GOBIN="${GOBIN:-$(go env GOBIN)}"
GOBIN="${GOBIN:-$(go env GOPATH | cut -d: -f1)/bin}"
ln -sf "${GOBIN}/lit" "${GOBIN}/lnks"
echo "Installed lit (lnks symlink created for compatibility)"
