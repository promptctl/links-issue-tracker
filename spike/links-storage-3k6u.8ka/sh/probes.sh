#!/bin/bash
for p in "$@"; do
  bash /tmp/minion-8ka/sh/x2.sh "$p" > "/tmp/minion-8ka/probe-$p.log" 2>&1
  sed -n '/== PROBE/,$p' "/tmp/minion-8ka/probe-$p.log" | grep -v "^  \|^$\|This is a blocking\|HOW TO RESOLVE\|ESCALATION\|build: dev\|lit sync reconcile \|agent-instructions\|purged on a side" | head -30
  echo
done
