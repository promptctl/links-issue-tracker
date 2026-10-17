#!/bin/bash
# Per-commit history cost at the 5x envelope (4955 issues): drive N ordinary
# writes into a perfbench-generated store, then re-root at several windows.
set -uo pipefail
N=${1:-600}
P=/tmp/minion-8ka/p5
/bin/rm -rf "$P"; /bin/cp -c -R /tmp/minion-8ka/perf/k/n=4955 "$P"
mkdir -p "$P-home/c" "$P-home/s" "$P-home/d"
export HOME="$P-home" XDG_CONFIG_HOME="$P-home/c" XDG_STATE_HOME="$P-home/s" XDG_DATA_HOME="$P-home/d" LIT_DISABLE_AUTO_SYNC=1 CLAUDE_CODE_SESSION_ID=
L=/tmp/minion-8ka/lit; H=/tmp/minion-8ka/hbin
cd "$P"
python3 - <<EOF
import json; p='$P/.git/links/dolt/links/.dolt/repo_state.json'; d=json.load(open(p)); print("remotes:", list(d['remotes'])); d['remotes']={}; json.dump(d,open(p,'w'),indent=2)
EOF
$H count "$P/.git/links/dolt"
IDS=($($H sql "$P/.git/links/dolt" "SELECT id FROM issues WHERE issue_type<>'epic' ORDER BY id LIMIT 400"))
echo "issues sampled: ${#IDS[@]}"
S=$(date +%s)
for i in $(seq 1 "$N"); do
  id=${IDS[$(( (i * 7919) % ${#IDS[@]} ))]}
  $L update "$id" --title "perf write $i" >/dev/null 2>&1 || echo "write $i failed"
done
echo "writes: $N in $(( $(date +%s) - S ))s"
$H count "$P/.git/links/dolt"
for W in full 0 100 300 "$N"; do
  D=$P-w$W; /bin/rm -rf "$D"; /bin/cp -c -R "$P" "$D"
  $H dropall "$D/.git/links/dolt/links" >/dev/null
  [ "$W" != full ] && $H rewrite "$D/.git/links/dolt/links" "$W" >/dev/null
  $H gc "$D/.git/links/dolt" >/dev/null
  echo "5x W=$W $($H count "$D/.git/links/dolt" | cut -d' ' -f1-3) noms=$(du -sk "$D/.git/links/dolt/links/.dolt/noms" | cut -f1)KiB"
done
