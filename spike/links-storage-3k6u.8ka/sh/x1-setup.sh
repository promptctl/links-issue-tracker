#!/bin/bash
# Two checkouts (alpha, bravo) sharing one bare git remote, with shared history.
set -euo pipefail
X=/tmp/minion-8ka/x1
/bin/rm -rf "$X"; mkdir -p "$X/home"; cd "$X"
export HOME="$X/home" LIT_DISABLE_AUTO_SYNC=1
git config --global user.email x@x.co; git config --global user.name x
git config --global init.defaultBranch master
L=/tmp/minion-8ka/lit
git init -q --bare remote.git
git clone -q remote.git alpha 2>/dev/null
cd alpha; echo hi > readme; git add -A; git commit -qm seed; git push -q origin HEAD 2>/dev/null
$L init --skip-hooks --skip-agents >/dev/null
EPIC=$($L new --type epic --title epic --topic demo | grep -oE 'links-[a-z0-9.-]+|[a-z]+-demo-[a-z0-9.]+' | head -1)
echo "epic=$EPIC"
$L new --type task --title shared-ticket --topic demo --parent "$EPIC"
$L sync push --set-upstream
cd "$X"; git clone -q remote.git bravo 2>/dev/null; cd bravo
$L init --skip-hooks --skip-agents
$L ls --all 2>&1 | head
