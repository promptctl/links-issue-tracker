#!/bin/bash
# Usage: x2.sh PROBE   (push|pull|receive|reconcile|adopt|mechanism)
# Builds alpha+bravo sharing a bare remote with shared history, makes a
# concurrent edit to one ticket on each side, then alpha re-roots its history
# (window 5) and force-pushes it. Then PROBE runs one sync entry point in bravo
# (or a fresh charlie for adopt) and prints what happened.
set -uo pipefail
PROBE=$1
X=/tmp/minion-8ka/x-$PROBE
/bin/rm -rf "$X"; mkdir -p "$X/home"; cd "$X"
export HOME="$X/home" LIT_DISABLE_AUTO_SYNC=1 CLAUDE_CODE_SESSION_ID=
git config --global user.email x@x.co; git config --global user.name x
git config --global init.defaultBranch master
L=/tmp/minion-8ka/lit
H=/tmp/minion-8ka/hbin
q() { "$@" >/dev/null 2>&1 || { echo "FAILED: $*"; "$@"; exit 1; }; }
git init -q --bare remote.git
git clone -q remote.git alpha 2>/dev/null
cd "$X/alpha"; echo hi > readme; git add -A; git commit -qm seed; git push -q origin HEAD 2>/dev/null
q $L init --skip-hooks --skip-agents
EPIC=$($L new --type epic --title epic --topic demo | grep -oE '[a-z]+-demo-[a-z0-9.]+' | head -1)
T=$($L new --type task --title shared-ticket --topic demo --parent "$EPIC" | grep -oE '[a-z]+-demo-[a-z0-9.]+' | head -1)
for i in $(seq 1 12); do q $L new --type task --title "filler-$i" --topic demo --parent "$EPIC"; done
q $L sync push --set-upstream
cd "$X"; git clone -q remote.git bravo 2>/dev/null
cd "$X/bravo"; q $L init --skip-hooks --skip-agents
# concurrent edits to the same ticket, different fields
q $L update "$T" --title "bravo-title"
cd "$X/alpha"; q $L update "$T" --description "alpha-description"
q $L sync push
echo "== alpha commits before rewrite: $($H count "$X/alpha/.git/links/dolt" | head -1)"
OUT=$($H rewrite "$X/alpha/.git/links/dolt/links" 5); echo "== alpha rewrite: $OUT"
CUT=$(echo "$OUT" | sed -E 's/.* cut=([a-z0-9]+) .*/\1/')
echo "== alpha force push:"; $L sync push --force 2>&1 | tail -3
echo "== alpha commits after: $($H count "$X/alpha/.git/links/dolt" | head -1)"
echo "== PROBE $PROBE"
case $PROBE in
  push) cd "$X/bravo"; $L sync push; echo "exit=$?" ;;
  pull) cd "$X/bravo"; $L sync pull; echo "exit=$?" ;;
  reconcile) cd "$X/bravo"; $L sync reconcile; echo "exit=$?" ;;
  receive) cd "$X/bravo"; $L sync __receive-bg --parent-pid 999999; echo "exit=$?"
     echo "-- receive.log:"; tail -5 .git/links/receive.log
     echo "-- status:"; $L sync status 2>&1 | head -20 ;;
  adopt) cd "$X"; git clone -q remote.git charlie 2>/dev/null; cd charlie
     $L init --skip-hooks --skip-agents; echo "exit=$?"
     $H count "$X/charlie/.git/links/dolt"; $L show "$T" 2>&1 | head -12 ;;
  mechanism) cd "$X/bravo"
     echo "-- bravo applies the same cut $CUT locally:"
     $H rewriteat "$X/bravo/.git/links/dolt/links" "$CUT"
     $L sync pull; echo "pull exit=$?"
     $L show "$T" 2>&1 | grep -E "^(bravo|alpha)-|title|description|alpha-description|bravo-title" | head
     $L sync push; echo "push exit=$?"
     cd "$X/alpha"; $L sync pull; echo "alpha pull exit=$?"
     $L show "$T" 2>&1 | grep -E "alpha-description|bravo-title" | head
     echo "alpha: $($H count "$X/alpha/.git/links/dolt")  bravo: $($H count "$X/bravo/.git/links/dolt")" ;;
esac
