#!/bin/bash
# Re-records the go test -json streams tools/testbudget's tests replay, each
# from the throwaway package beside this script. slow must pass; the timeout
# packages hang on purpose and must fail, and their stream is the product.
# -trimpath keeps machine paths out of the recorded stack traces.
set -u
cd "$(dirname "$0")" || exit 1
go -C slow test -json -count=1 -trimpath . > ../slow.jsonl || {
	echo "record.sh: slow failed; ../slow.jsonl is not a passing stream" >&2
	exit 1
}
for pkg in timeout-serial timeout-parallel; do
	go -C "$pkg" test -json -count=1 -trimpath -timeout 2s . > "../$pkg.jsonl"
	grep -q 'panic: test timed out after 2s' "../$pkg.jsonl" || {
		echo "record.sh: $pkg did not hit its -timeout; ../$pkg.jsonl is not a timeout stream" >&2
		exit 1
	}
done
