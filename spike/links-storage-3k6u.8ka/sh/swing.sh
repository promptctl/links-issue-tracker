#!/bin/bash
# Shrink the git remote without reading through git: push the re-rooted, locally
# GC'd store to a FRESH data ref, then compare-and-swap refs/dolt/data onto it.
set -uo pipefail
cd /tmp/minion-8ka
H=./hbin
R=/tmp/minion-8ka/rs2-remote.git
D=/tmp/minion-8ka/rs2
/bin/rm -rf "$R" "$D"
/bin/cp -c -R rs-remote.git "$R"   # copy of the remote: full history pushed, then the W=500 re-root force-pushed
/bin/cp -c -R rs "$D"              # the store that did it (re-rooted at W=500, GC'd)
/bin/rm -rf "$D/.git/links/dolt/links/.dolt/git-remote-cache"
python3 - <<EOF
import json; p='$D/.git/links/dolt/links/.dolt/repo_state.json'; d=json.load(open(p)); d['remotes']={}; json.dump(d,open(p,'w'),indent=2)
EOF
tree() {
  for r in $(git --git-dir="$R" for-each-ref --format='%(refname)'); do
    echo "   $r $(git --git-dir="$R" rev-parse --short "$r") files=$(git --git-dir="$R" ls-tree -r "$r" | wc -l | tr -d ' ') bytes=$(git --git-dir="$R" ls-tree -r --long "$r" | awk '{s+=$4} END {print s}')"
  done
}
echo "before:"; tree
echo "local noms: $(du -sk "$D/.git/links/dolt/links/.dolt/noms" | cut -f1) KiB"; $H count "$D/.git/links/dolt"
$H sql "$D/.git/links/dolt" "CALL DOLT_REMOTE('add','--ref','refs/dolt/compact','compact','git+file://$R')" >/dev/null
S=$(date +%s)
$H sql "$D/.git/links/dolt" "CALL DOLT_PUSH('compact','master')"
echo "push to fresh ref: $(( $(date +%s) - S ))s"
OLD=$(git --git-dir="$R" rev-parse refs/dolt/data)
NEW=$(git --git-dir="$R" rev-parse refs/dolt/compact)
git --git-dir="$R" update-ref refs/dolt/data "$NEW" "$OLD" && echo "CAS swing refs/dolt/data $OLD -> $NEW ok"
git --git-dir="$R" update-ref -d refs/dolt/compact
echo "after swing:"; tree
git --git-dir="$R" gc -q --prune=now; echo "remote on disk after git gc: $(du -sk "$R" | cut -f1) KiB"
/bin/rm -rf /tmp/minion-8ka/rs2-clone; mkdir -p /tmp/minion-8ka/rs2-clone
S=$(date +%s)
$H sql /tmp/minion-8ka/rs2-clone "CALL DOLT_CLONE('git+file://$R','links')"
echo "fresh clone from swung remote: $(( $(date +%s) - S ))s"
$H count /tmp/minion-8ka/rs2-clone
echo "clone noms: $(du -sk /tmp/minion-8ka/rs2-clone/links/.dolt/noms | cut -f1) KiB  mirror: $(du -sk /tmp/minion-8ka/rs2-clone/links/.dolt/git-remote-cache | cut -f1) KiB"
$H sql /tmp/minion-8ka/rs2-clone "SELECT COUNT(*) FROM issues"
