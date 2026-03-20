install:
    ./scripts/install.sh

build:
    # [LAW:one-source-of-truth] both entrypoints build from the same CLI implementation so compatibility cannot drift.
    go build -buildvcs=false ./cmd/lit ./cmd/lnks
