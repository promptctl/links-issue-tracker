#!/bin/bash
# Full GC against the git remote left by remote-size.sh (re-rooted at W=500 and force-pushed).
set -uo pipefail
R=/tmp/minion-8ka/rs-remote.git
tree() {
  for r in $(git --git-dir="$R" for-each-ref --format='%(refname)'); do
    echo "   $r files=$(git --git-dir="$R" ls-tree -r "$r" | wc -l | tr -d ' ') bytes=$(git --git-dir="$R" ls-tree -r --long "$r" | awk '{s+=$4} END {print s}')"
  done
}
echo "before:"; tree
/bin/rm -rf /tmp/minion-8ka/rgc-cache; mkdir -p /tmp/minion-8ka/rgc-cache
/tmp/minion-8ka/hbin remotegc "git+file://$R" /tmp/minion-8ka/rgc-cache
echo "after:"; tree
git --git-dir="$R" gc -q --prune=now; echo "remote on disk after git gc: $(du -sk "$R" | cut -f1) KiB"
# a fresh clone of the GC'd remote
/bin/rm -rf /tmp/minion-8ka/rgc-clone; mkdir -p /tmp/minion-8ka/rgc-clone
S=$(date +%s)
/tmp/minion-8ka/hbin sql /tmp/minion-8ka/rgc-clone "CALL DOLT_CLONE('git+file://$R','links')"
echo "fresh clone: $(( $(date +%s) - S ))s"
/tmp/minion-8ka/hbin count /tmp/minion-8ka/rgc-clone
echo "clone noms: $(du -sk /tmp/minion-8ka/rgc-clone/links/.dolt/noms | cut -f1) KiB  mirror: $(du -sk /tmp/minion-8ka/rgc-clone/links/.dolt/git-remote-cache 2>/dev/null | cut -f1) KiB"
