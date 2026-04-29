install:
    ./scripts/install.sh

build:
    go build -buildvcs=false ./cmd/lit
