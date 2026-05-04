#!/usr/bin/env bash
# Reads session_id from Claude Code SessionStart hook stdin JSON and emits identity guidance.
set -euo pipefail

input=$(cat)
match=$(echo "$input" | grep -o '"session_id":"[^"]*"' || true)
session_id=$(echo "$match" | head -1 | cut -d'"' -f4)

if [ -n "$session_id" ]; then
    echo "Your Claude Code session id is: ${session_id}. When using lit, your assignee identity is claude_${session_id}."
fi
