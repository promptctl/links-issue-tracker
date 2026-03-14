#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

GOFLAGS="${GOFLAGS:+$GOFLAGS }-buildvcs=false" go install ./cmd/lnks

# Backward-compat symlink: lit → lnks (deprecated, removal: 2026-09-01)
GOBIN="${GOBIN:-$(go env GOBIN)}"
GOBIN="${GOBIN:-$(go env GOPATH | cut -d: -f1)/bin}"
ln -sf "${GOBIN}/lnks" "${GOBIN}/lit"
echo "Installed lnks (lit symlink created for backward compatibility, will be removed 2026-09-01)"
