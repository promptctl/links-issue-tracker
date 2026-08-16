# shellcheck shell=sh
# Where a Go module's source actually lives — the single home of that mapping.
# Sourced by cleanroom-sandbox.sh and cleanroom-reach-probe.sh, which previously each
# re-derived it and each got it wrong: the sandbox denied paths the probe never looked
# at, and the probe reported "blocked" for modules it had simply failed to locate.
#
# Two rules the go command applies that hand-written paths routinely miss:
#
#   1. The directory is <full module path>@<version>, major-version suffix INCLUDED.
#      github.com/hashicorp/golang-lru/v2 v2.0.7 -> .../golang-lru/v2@v2.0.7
#      github.com/dolthub/fslock         v0.0.3  -> .../fslock@v0.0.3
#      Stripping the /vN suffix finds the v2 tree and misses every v0/v1 module.
#   2. Uppercase letters are case-encoded as !<lowercase>, because the cache must be
#      safe on case-insensitive filesystems.
#      github.com/BurntSushi/toml -> github.com/!burnt!sushi/toml
#      The same encoding applies to proxy.golang.org URLs, which reject the raw path.

# Case-encode a module path the way the go command does.
cleanroom_escape_module() {
	printf '%s' "$1" | sed 's/\([A-Z]\)/!\1/g' | tr '[:upper:]' '[:lower:]'
}

# Strip a trailing major-version element, so v1 and v2 of one library share a base.
# github.com/hashicorp/golang-lru/v2 -> github.com/hashicorp/golang-lru
cleanroom_module_base() {
	printf '%s' "$1" | sed -E 's|/v[0-9]+$||'
}

# Absolute path of an unpacked module in the cache.
cleanroom_module_dir() {
	printf '%s/%s@%s' "$(go env GOMODCACHE)" "$(cleanroom_escape_module "$1")" "$2"
}

# Escape a literal path for embedding in a seatbelt (regex #"...") rule.
cleanroom_regex_quote() {
	printf '%s' "$1" | sed -e 's/[.[\*^$+?(){}|]/\\&/g'
}
