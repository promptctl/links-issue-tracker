G=/tmp/minion-8ka/live/.git/links/dolt/links/.dolt/git-remote-cache/0a69e895025c221a9ea7a8570fba375f7f42b82be0f8478d65714c6f1c854737/repo.git
R=$(git --git-dir="$G" for-each-ref --sort=-committerdate --format='%(refname)' | head -1)
echo "newest ref: $R"
git --git-dir="$G" log --format='%h %ci parents=%p %s' -5 "$R"
echo "chain length:"; git --git-dir="$G" rev-list --count "$R"
git --git-dir="$G" ls-tree -r --long "$R" | sort -k4 -n | tail -15
echo "entries:"; git --git-dir="$G" ls-tree -r "$R" | wc -l
echo "tree size sum:"; git --git-dir="$G" ls-tree -r --long "$R" | awk '{s+=$4} END {print s}'
echo "distinct commits across all refs:"; git --git-dir="$G" rev-list --all | wc -l
