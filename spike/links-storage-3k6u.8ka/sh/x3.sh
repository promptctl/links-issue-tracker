#!/bin/bash
# Full cycle: alpha re-roots (window W), force-pushes, GCs, swings the remote to a
# compacted data ref. bravo (diverged above the cut) applies the same cut and
# syncs through the background receive, push and pull. delta (diverged BELOW the
# cut, never synced since) shows the case the same-cut replay cannot reach.
set -uo pipefail
X=/tmp/minion-8ka/x3
W=${1:-10}
/bin/rm -rf "$X"; mkdir -p "$X/home"; cd "$X"
export HOME="$X/home" LIT_DISABLE_AUTO_SYNC=1 CLAUDE_CODE_SESSION_ID=
git config --global user.email x@x.co; git config --global user.name x
git config --global init.defaultBranch master
L=/tmp/minion-8ka/lit; H=/tmp/minion-8ka/hbin
q() { "$@" >/dev/null 2>&1 || { echo "FAILED: $*"; "$@"; exit 1; }; }
ids() { grep -oE '[a-z]+-demo-[a-z0-9.]+' | head -1; }
tree() { for r in $(git --git-dir="$X/remote.git" for-each-ref --format='%(refname)' refs/dolt); do echo "   $r files=$(git --git-dir="$X/remote.git" ls-tree -r "$r" | wc -l | tr -d ' ') bytes=$(git --git-dir="$X/remote.git" ls-tree -r --long "$r" | awk '{s+=$4} END {print s}')"; done; }
git init -q --bare remote.git
git clone -q remote.git alpha 2>/dev/null
cd "$X/alpha"; echo hi > readme; git add -A; git commit -qm seed; git push -q origin HEAD 2>/dev/null
q $L init --skip-hooks --skip-agents
EPIC=$($L new --type epic --title epic --topic demo | ids)
T=$($L new --type task --title shared-ticket --topic demo --parent "$EPIC" | ids)
T2=$($L new --type task --title stale-ticket --topic demo --parent "$EPIC" | ids)
q $L sync push --set-upstream
cd "$X"; git clone -q remote.git delta 2>/dev/null; cd "$X/delta"; q $L init --skip-hooks --skip-agents
q $L update "$T2" --title "delta-title-unpushed"     # delta forks here and goes quiet
cd "$X/alpha"
for i in $(seq 1 $((W * 3))); do q $L new --type task --title "filler-$i" --topic demo --parent "$EPIC"; done
q $L sync push
cd "$X"; git clone -q remote.git bravo 2>/dev/null; cd "$X/bravo"; q $L init --skip-hooks --skip-agents
q $L update "$T" --title "bravo-title"                # bravo's concurrent edit, unpushed
cd "$X/alpha"; q $L update "$T" --description "alpha-description"; q $L sync push
echo "== before: alpha $($H count "$X/alpha/.git/links/dolt")"; tree
# ---- the bound, run by alpha ----
OUT=$($H rewrite "$X/alpha/.git/links/dolt/links" "$W"); echo "== alpha re-root: $OUT"
CUT=$(echo "$OUT" | sed -E 's/.* cut=([a-z0-9]+) .*/\1/')
q $L sync push --force
$H dropall "$X/alpha/.git/links/dolt/links" >/dev/null   # pre-migrate-* etc. pin old history
$H gc "$X/alpha/.git/links/dolt" >/dev/null
q $H sql "$X/alpha/.git/links/dolt" "CALL DOLT_REMOTE('add','--ref','refs/dolt/compact','compact','git+file://$X/remote.git')"
q $H sql "$X/alpha/.git/links/dolt" "CALL DOLT_PUSH('compact','master')"
q $H sql "$X/alpha/.git/links/dolt" "CALL DOLT_REMOTE('remove','compact')"
OLD=$(git --git-dir="$X/remote.git" rev-parse refs/dolt/data); NEW=$(git --git-dir="$X/remote.git" rev-parse refs/dolt/compact)
git --git-dir="$X/remote.git" update-ref refs/dolt/data "$NEW" "$OLD" && echo "== swing CAS ok"
git --git-dir="$X/remote.git" update-ref -d refs/dolt/compact
echo "== after swing:"; tree
echo "== alpha after: $($H count "$X/alpha/.git/links/dolt")"
# ---- bravo: diverged above the cut ----
echo "== bravo applies cut $CUT:"; $H rewriteat "$X/bravo/.git/links/dolt/links" "$CUT"
cd "$X/bravo"
$L sync __receive-bg --parent-pid 999999 >/dev/null 2>&1; echo "bravo background receive exit=$? pending-block=$(ls .git/links/receive-block.pending 2>/dev/null || echo none)"
$L show "$T" 2>&1 | grep -E "bravo-title|alpha-description"
$L sync push 2>&1 | tail -1; echo "bravo push exit=$?"
cd "$X/alpha"; $L sync pull 2>&1 | tail -1; echo "alpha pull exit=$?"; $L show "$T" 2>&1 | grep -E "bravo-title|alpha-description"
q $L new --type task --title after-bound --topic demo --parent "$EPIC"; $L sync push 2>&1 | tail -1
cd "$X/bravo"; $L sync pull 2>&1 | tail -1; echo "bravo pull exit=$?"; $L ls 2>&1 | grep -c after-bound
echo "== alpha $($H count "$X/alpha/.git/links/dolt")  bravo $($H count "$X/bravo/.git/links/dolt")"
# ---- charlie: fresh adopt after the swing ----
cd "$X"; git clone -q remote.git charlie 2>/dev/null; cd "$X/charlie"; q $L init --skip-hooks --skip-agents
echo "== charlie adopt: $($H count "$X/charlie/.git/links/dolt")"; $L show "$T" 2>&1 | grep -E "bravo-title|alpha-description"
# ---- delta: forked below the cut ----
cd "$X/delta"
echo "== delta has the cut commit? $($H sql "$X/delta/.git/links/dolt" "SELECT COUNT(*) FROM dolt_log() WHERE commit_hash='$CUT'")"
$L sync pull 2>&1 | grep -E "WHAT HAPPENED|error \(code" | head -2; echo "delta pull exit=$?"
