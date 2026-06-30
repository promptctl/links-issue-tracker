# lit build recipes. Building from source links the embedded Dolt engine via
# cgo, which needs ICU/zstd on the toolchain search path. scripts/cgo-env.sh is
# the single source of truth for those paths (a no-op off macOS); every recipe
# that compiles sources it so the flags can never drift between entrypoints.
# [LAW:single-enforcer]

# List available recipes.
default:
    @just --list

# One-time per machine: install the native build deps (macOS) and persist the
# cgo ICU/zstd search paths into `go env`, so plain `go build` / `go test` and
# your IDE all work afterward without sourcing anything. Idempotent.
setup:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ "$(uname -s)" = "Darwin" ]; then
        command -v brew >/dev/null || { echo "Install Homebrew first: https://brew.sh" >&2; exit 1; }
        brew install icu4c@78 zstd
    fi
    source "{{justfile_directory()}}/scripts/cgo-env.sh"
    if [ -n "${CGO_CPPFLAGS:-}" ]; then
        go env -w CGO_CPPFLAGS="$CGO_CPPFLAGS" CGO_LDFLAGS="$CGO_LDFLAGS"
        echo "Persisted ICU/zstd cgo paths into 'go env' — plain go build/test now work."
    else
        echo "No extra cgo flags needed on this platform; go build/test work as-is."
    fi

# Build the lit binary.
build:
    #!/usr/bin/env bash
    set -euo pipefail
    source "{{justfile_directory()}}/scripts/cgo-env.sh"
    go build -buildvcs=false ./cmd/lit

# Run the test suite. With no args runs the whole suite; otherwise passes args
# through, e.g. `just test -run TestFoo ./internal/cli/`.
test *args:
    #!/usr/bin/env bash
    set -euo pipefail
    source "{{justfile_directory()}}/scripts/cgo-env.sh"
    args="{{args}}"
    if [ -z "$args" ]; then
        go test ./...
    else
        go test $args
    fi

# Lint (depguard lifecycle-boundary rule + style). Needs golangci-lint installed.
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    source "{{justfile_directory()}}/scripts/cgo-env.sh"
    golangci-lint run

# Build from source and install onto your PATH.
install:
    ./scripts/install.sh
