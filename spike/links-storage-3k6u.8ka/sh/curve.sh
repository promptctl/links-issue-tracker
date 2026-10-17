#!/bin/bash
# Size curve: re-root master at window W, drop every other ref, full GC, measure.
set -e
cd /tmp/minion-8ka
for W in "$@"; do
  D=/tmp/minion-8ka/c$W
  /bin/rm -rf "$D"
  /bin/cp -c -R live "$D"
  grep -q '"remotes": {}' "$D/.git/links/dolt/links/.dolt/repo_state.json" || { echo "remotes not stripped"; exit 1; }
  /bin/rm -rf "$D/.git/links/dolt/links/.dolt/git-remote-cache"
  ./hbin dropall "$D/.git/links/dolt/links" >/dev/null
  if [ "$W" != "full" ]; then ./hbin rewrite "$D/.git/links/dolt/links" "$W"; fi
  ./hbin gc "$D/.git/links/dolt"
  ./hbin count "$D/.git/links/dolt"
  echo "W=$W noms_bytes=$(du -sk "$D/.git/links/dolt/links/.dolt/noms" | cut -f1)KiB"
done
