#!/bin/bash
# Re-records the go test -json stream tools/testshape's timing test replays,
# from the throwaway package beside this script. It must pass.
set -u
cd "$(dirname "$0")" || exit 1
go -C timed test -json -count=1 -trimpath . > ../timed.jsonl || {
	echo "record.sh: timed failed; ../timed.jsonl is not a passing stream" >&2
	exit 1
}
