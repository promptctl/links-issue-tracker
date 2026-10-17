cd /tmp/minion-8ka/live/.git/links/dolt/links/.dolt/git-remote-cache
for d in */; do
  echo "== $d"; du -sh "$d"; G="$d/repo.git"
  git --git-dir="$G" for-each-ref
  git --git-dir="$G" count-objects -vH
  echo "commits on refs/dolt/data:"; git --git-dir="$G" rev-list --count refs/dolt/data 2>&1
  git --git-dir="$G" log --format='%h %ci %s' -3 refs/dolt/data 2>&1
  git --git-dir="$G" ls-tree -r --long refs/dolt/data 2>&1 | head -20
  git --git-dir="$G" ls-tree -r refs/dolt/data | wc -l
done
