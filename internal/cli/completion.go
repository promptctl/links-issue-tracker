package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// commandCompletionModel projects the command registry into the name/summary/
// subcommand tree the shell-completion generators render. It reads only the
// static metadata of each CommandSpec; the Run closures are never invoked, so
// the throwaway context and writers here exist only to satisfy commandSpecs'
// constructor signature. The registry is the single place command and
// subcommand names are declared, so a generated script cannot omit a command
// the registry has nor invent one it lacks. [LAW:one-source-of-truth]
func commandCompletionModel() []CommandSpec {
	specs := commandSpecs(context.Background(), io.Discard, io.Discard)
	// help is cobra's built-in command (only the default completion command is
	// disabled on root, not help), so it is a real invocable command the
	// registry does not own. Listing it keeps completion faithful to the actual
	// command surface rather than to the registry alone.
	return append(specs, CommandSpec{Name: "help", Summary: "Help about any command"})
}

func renderBashCompletion() string { return generateBashCompletion(commandCompletionModel()) }
func renderZshCompletion() string  { return generateZshCompletion(commandCompletionModel()) }
func renderFishCompletion() string { return generateFishCompletion(commandCompletionModel()) }

// completionRenderer maps a validated shell name to its generator. resolve in
// runCompletion has already rejected any name not in completionFamily, so an
// unmapped name here means completionFamily and this switch drifted — fail
// loudly rather than emit an empty script. [LAW:no-silent-failure]
func completionRenderer(shell string) func() string {
	switch shell {
	case "bash":
		return renderBashCompletion
	case "zsh":
		return renderZshCompletion
	case "fish":
		return renderFishCompletion
	}
	panic(fmt.Sprintf("completion: no renderer for shell %q", shell))
}

// familyNode is a (trigger word -> child names) pair for the flat shell models
// (bash keys on the preceding word, fish on any seen subcommand word). The
// command tree is flattened to these pairs at any depth.
type familyNode struct {
	name     string
	children []string
}

// topLevelNames is the ordered command list a bare `lit <tab>` completes.
func topLevelNames(cmds []CommandSpec) []string {
	names := make([]string, len(cmds))
	for i, c := range cmds {
		names[i] = c.Name
	}
	return names
}

func subNames(subs []SubcommandSpec) []string {
	names := make([]string, len(subs))
	for i, s := range subs {
		names[i] = s.Name
	}
	return names
}

// familyNodes flattens every command/subcommand that has children into a
// trigger-word list, deduplicating by name so the flat shell models emit one
// arm per word. A word that legally appears under two parents (e.g. `label`
// under both the top-level command and `bulk`) merges its children, so the
// completion is a superset and never omits a valid value. [LAW:no-silent-failure]
func familyNodes(cmds []CommandSpec) []familyNode {
	var flat []familyNode
	var walk func(name string, subs []SubcommandSpec)
	walk = func(name string, subs []SubcommandSpec) {
		if len(subs) == 0 {
			return
		}
		flat = append(flat, familyNode{name: name, children: subNames(subs)})
		for _, s := range subs {
			walk(s.Name, s.Subcommands)
		}
	}
	for _, c := range cmds {
		walk(c.Name, c.Subcommands)
	}

	order := make([]string, 0, len(flat))
	byName := make(map[string][]string, len(flat))
	for _, n := range flat {
		if _, seen := byName[n.name]; !seen {
			order = append(order, n.name)
		}
		byName[n.name] = unionPreservingOrder(byName[n.name], n.children)
	}
	nodes := make([]familyNode, len(order))
	for i, name := range order {
		nodes[i] = familyNode{name: name, children: byName[name]}
	}
	return nodes
}

func unionPreservingOrder(existing, incoming []string) []string {
	seen := make(map[string]bool, len(existing))
	for _, e := range existing {
		seen[e] = true
	}
	out := existing
	for _, v := range incoming {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func generateBashCompletion(cmds []CommandSpec) string {
	var b strings.Builder
	b.WriteString("# bash completion for lit\n")
	b.WriteString("_lit_completions() {\n")
	b.WriteString("  local current prev words cword\n")
	b.WriteString("  _init_completion || return\n\n")
	fmt.Fprintf(&b, "  local commands=%q\n\n", strings.Join(topLevelNames(cmds), " "))
	b.WriteString("  case \"${prev}\" in\n")
	b.WriteString("    lit)\n")
	b.WriteString("      COMPREPLY=( $(compgen -W \"${commands}\" -- \"${current}\") )\n")
	b.WriteString("      return\n      ;;\n")
	for _, n := range familyNodes(cmds) {
		fmt.Fprintf(&b, "    %s)\n", n.name)
		fmt.Fprintf(&b, "      COMPREPLY=( $(compgen -W %q -- \"${current}\") )\n", strings.Join(n.children, " "))
		b.WriteString("      return\n      ;;\n")
	}
	b.WriteString("  esac\n\n")
	b.WriteString("  COMPREPLY=( $(compgen -W \"${commands}\" -- \"${current}\") )\n")
	b.WriteString("}\n\ncomplete -F _lit_completions lit\n")
	return b.String()
}

func generateZshCompletion(cmds []CommandSpec) string {
	var b strings.Builder
	b.WriteString("#compdef lit\n\n")
	b.WriteString("_lit() {\n")
	b.WriteString("  local -a commands\n")
	b.WriteString("  commands=(\n")
	for _, c := range cmds {
		fmt.Fprintf(&b, "    %s\n", zshDescribeEntry(c.Name, c.Summary))
	}
	b.WriteString("  )\n\n")
	b.WriteString("  local context state state_descr line\n")
	b.WriteString("  _arguments '1:command:->command' '2:subcommand:->subcommand'\n\n")
	b.WriteString("  case $state in\n")
	b.WriteString("    command)\n      _describe 'command' commands\n      ;;\n")
	b.WriteString("    subcommand)\n      case $line[1] in\n")
	for _, c := range cmds {
		if len(c.Subcommands) == 0 {
			continue
		}
		fmt.Fprintf(&b, "        %s)\n          _values '%s commands' %s\n          ;;\n",
			c.Name, c.Name, strings.Join(subNames(c.Subcommands), " "))
	}
	b.WriteString("      esac\n      ;;\n  esac\n}\n\n_lit \"$@\"\n")
	return b.String()
}

// zshDescribeEntry renders a 'name:description' completion entry. zsh splits the
// name from the description on the first colon, so colons inside the summary are
// safe; single quotes are not, and must be escaped or the array literal breaks.
func zshDescribeEntry(name, summary string) string {
	return "'" + name + ":" + strings.ReplaceAll(summary, "'", `'\''`) + "'"
}

func generateFishCompletion(cmds []CommandSpec) string {
	var b strings.Builder
	b.WriteString("complete -c lit -f\n")
	fmt.Fprintf(&b, "complete -c lit -n '__fish_use_subcommand' -a '%s'\n", strings.Join(topLevelNames(cmds), " "))
	for _, n := range familyNodes(cmds) {
		fmt.Fprintf(&b, "complete -c lit -n '__fish_seen_subcommand_from %s' -a '%s'\n",
			n.name, strings.Join(n.children, " "))
	}
	return b.String()
}
