#!/bin/bash
set -uo pipefail
X=/tmp/minion-8ka/rs2-adopt
/bin/rm -rf "$X"; mkdir -p "$X/home"; cd "$X"
export HOME="$X/home" LIT_DISABLE_AUTO_SYNC=1 CLAUDE_CODE_SESSION_ID=
git config --global user.email x@x.co; git config --global user.name x
git clone -q /tmp/minion-8ka/rs2-remote.git checkout 2>/dev/null; cd checkout
S=$(date +%s)
/tmp/minion-8ka/lit init --skip-hooks --skip-agents 2>&1 | head -4
echo "adopt: $(( $(date +%s) - S ))s"
/tmp/minion-8ka/hbin count "$X/checkout/.git/links/dolt"
echo "noms: $(du -sk .git/links/dolt/links/.dolt/noms | cut -f1) KiB  mirror: $(du -sk .git/links/dolt/links/.dolt/git-remote-cache | cut -f1) KiB"
/tmp/minion-8ka/lit show links-storage-3k6u 2>&1 | head -2
/tmp/minion-8ka/lit backlog 2>&1 | head -3
