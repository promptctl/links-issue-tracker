#!/bin/bash
# Re-records the go test -json streams tools/testbudget's tests replay, each
# from the throwaway package beside this script. The timeout packages hang on
# purpose, so their go test exits nonzero; the stream is the product.
set -u
cd "$(dirname "$0")" || exit 1
go -C slow test -json -count=1 . > ../slow.jsonl
go -C timeout-serial test -json -count=1 -timeout 2s . > ../timeout-serial.jsonl
go -C timeout-parallel test -json -count=1 -timeout 2s . > ../timeout-parallel.jsonl
