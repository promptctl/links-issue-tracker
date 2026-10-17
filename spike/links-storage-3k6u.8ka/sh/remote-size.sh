#!/bin/bash
# Does the git-backed remote ever shrink after a re-root + full GC + force push?
set -uo pipefail
cd /tmp/minion-8ka
H=./hbin
R=/tmp/minion-8ka/rs-remote.git
D=/tmp/minion-8ka/rs
/bin/rm -rf "$R" "$D"
git init -q --bare "$R"
/bin/rm -rf /tmp/minion-8ka/rs-seed; git clone -q "$R" /tmp/minion-8ka/rs-seed 2>/dev/null
git -C /tmp/minion-8ka/rs-seed -c user.email=x@x.co -c user.name=x commit -q --allow-empty -m seed
git -C /tmp/minion-8ka/rs-seed push -q origin HEAD:master 2>/dev/null
/bin/cp -c -R live "$D"
grep -q '"remotes": {}' "$D/.git/links/dolt/links/.dolt/repo_state.json" || { echo "remotes not stripped"; exit 1; }
/bin/rm -rf "$D/.git/links/dolt/links/.dolt/git-remote-cache"
$H dropall "$D/.git/links/dolt/links"
$H gc "$D/.git/links/dolt"
$H sql "$D/.git/links/dolt" "CALL DOLT_REMOTE('add','origin','git+file://$R')"
tree() {
  echo "-- remote: $(du -sk "$R" | cut -f1) KiB on disk; data ref tree:"
  for r in $(git --git-dir="$R" for-each-ref --format='%(refname)'); do
    echo "   $r files=$(git --git-dir="$R" ls-tree -r "$r" | wc -l | tr -d ' ') bytes=$(git --git-dir="$R" ls-tree -r --long "$r" | awk '{s+=$4} END {print s}')"
  done
}
S=$(date +%s)
$H sql "$D/.git/links/dolt" "CALL DOLT_PUSH('origin','master')"
echo "push full: $(( $(date +%s) - S ))s"; tree
$H rewrite "$D/.git/links/dolt/links" 500
$H gc "$D/.git/links/dolt"
echo "local noms after rewrite+gc: $(du -sk "$D/.git/links/dolt/links/.dolt/noms" | cut -f1) KiB"
S=$(date +%s)
$H sql "$D/.git/links/dolt" "CALL DOLT_PUSH('--force','origin','master')"
echo "force push: $(( $(date +%s) - S ))s"; tree
echo "-- local mirror cache: $(du -sk "$D/.git/links/dolt/links/.dolt/git-remote-cache" | cut -f1) KiB"
$H dropall "$D/.git/links/dolt/links"
$H gc "$D/.git/links/dolt"
echo "local noms after push+dropall+gc: $(du -sk "$D/.git/links/dolt/links/.dolt/noms" | cut -f1) KiB"
S=$(date +%s)
$H sql "$D/.git/links/dolt" "CALL DOLT_BACKUP('sync-url','git+file://$R')"
echo "backup sync-url: $(( $(date +%s) - S ))s"; tree
git --git-dir="$R" gc -q --prune=now; echo "after git gc: $(du -sk "$R" | cut -f1) KiB"
