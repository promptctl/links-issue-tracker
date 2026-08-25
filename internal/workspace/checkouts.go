package workspace

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Checkout is one working tree of this repository as the machine can see it at
// this instant: the identity it has minted, and where it is. Nothing here is
// stored — the value is re-derived on every enumeration and thrown away, which
// is the whole reason identity lives in the private git dir. A registry would
// be a second representation of a fact git already owns, and it would outlive
// the worktrees it described. [LAW:one-source-of-truth]
//
// Stream is absent for a checkout that has never mutated: a real state, not a
// gap. Such a checkout has produced no work events, so it can hold no claim,
// and it is correctly invisible to the liveness prune. Absent is not dead.
//
// Path and Branch travel with the token because a claim is rendered where its
// address can be resolved: a claimant that is a live worktree of this store on
// this machine can be walked over to, and the enumeration that proves it alive
// is the same one that knows where it is. They stay on this machine — the
// shared database admits only opaque discriminators (design-docs/work-claims.md,
// "The privacy invariant"). Branch is empty for a detached HEAD.
type Checkout struct {
	Stream StreamID
	Path   string
	Branch string
}

// LiveCheckouts enumerates every working tree of the repository containing cwd,
// with the identity each one holds. It is the local half of the claim
// predicate's liveness leg: a stream token that appears in the shared evidence
// under this workspace's id but in none of these checkouts belongs to a checkout
// that no longer exists, and its claims are void here immediately rather than
// waiting out the freshness window. The asymmetry is deliberate — deletion is a
// local fact, and only its owner can observe it instantly.
//
// [LAW:one-source-of-truth] Git owns which worktrees exist, so this asks git
// rather than inferring it from the filesystem. The difference is not academic:
// a worktree deleted with `rm -rf` leaves its private git directory — token and
// all — behind under <git-common-dir>/worktrees/, so a directory listing would
// report a checkout that has been gone for months as alive. Git reports it
// `prunable`. And a worktree that has been explicitly locked is NOT prunable
// even while its directory is missing, which is the removable-media case: git
// declines to call it dead, and so does this. Inheriting git's judgment on both
// is what keeps this leg from ever voiding a checkout that is merely out of
// reach.
//
// [LAW:effects-at-boundaries] The git subprocesses and the token reads are the
// only effects; the result is a plain value, and claim derivation — which
// consumes it — reads no filesystem at all.
//
// [LAW:no-silent-failure] A checkout git lists as live but whose identity cannot
// be reached fails the whole enumeration. Dropping it instead would leave it out
// of the live set, which is not "we skipped one" — it is this machine asserting
// the checkout is deleted, and voiding a live claimant's evidence on the
// strength of an error it declined to mention.
func LiveCheckouts(cwd string) ([]Checkout, error) {
	// [LAW:dataflow-not-control-flow] A local enumeration that cannot block on a
	// network, so context.Background() — "never cancels" — is the honest value,
	// matching every other git query in this package. See Resolve for the note.
	output, err := gitOutput(context.Background(), cwd, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		// Deliberately NOT routed through classifyGitError. That function mints
		// the ErrNotGitRepo sentinel, which means "this directory is not a
		// lit-usable repository" — the answer a repo GATE gives, and Discover
		// skips silently on it. Every caller here has already passed such a gate,
		// so the sentinel could only be a lie, and a caller that treated it the
		// way Discover does would read a failed enumeration as an empty one and
		// void every claim this workspace holds. [LAW:no-silent-failure]
		//
		// The -z requirement is stated on EVERY failure rather than on a matched
		// exit code, for the reason publishStreamToken states its hard-link
		// requirement the same way: it is true of every failure, so naming it is
		// never wrong, while classifying would go quiet on the operator who most
		// needs the explanation. Git before 2.36 rejects the flag outright, which
		// is the one failure here with a one-line fix.
		return nil, fmt.Errorf("git worktree list --porcelain -z in %q: %w; this step requires git 2.36 or newer, which is where `-z` arrived — the unterminated format it replaces cannot be parsed correctly for worktree paths containing a newline", cwd, err)
	}
	records, err := parseWorktreeList(output)
	if err != nil {
		return nil, fmt.Errorf("enumerate worktrees in %q: %w", cwd, err)
	}
	// Filtered as data rather than skipped inside the loop below, so the read
	// that follows runs identically for every record that reaches it. Unlike
	// claims.standingOf, this does not clone first: records was parsed a line
	// ago and has no other reader to consume out from under.
	// [LAW:dataflow-not-control-flow]
	inhabited := slices.DeleteFunc(records, worktreeRecord.uninhabited)
	checkouts := make([]Checkout, 0, len(inhabited))
	for _, record := range inhabited {
		// The same query Resolve uses for the current checkout, pointed at
		// another one. --git-dir answers with the checkout's OWN directory —
		// ".git" in the primary clone, ".git/worktrees/<name>" in a linked one —
		// so asking it from each worktree's path is the whole resolution, and no
		// second spelling of "where does this checkout keep its identity" exists
		// to drift from Resolve's. [LAW:single-enforcer]
		privateGitDir, err := resolvePrivateGitDir(record.path)
		if err != nil {
			return nil, fmt.Errorf("locate the git directory of worktree %q, which git lists as live: %w", record.path, err)
		}
		stream, err := ReadStream(privateGitDir)
		if err != nil {
			return nil, err
		}
		checkouts = append(checkouts, Checkout{Stream: stream, Path: record.path, Branch: record.branch})
	}
	return checkouts, nil
}

// worktreeRecord is one `git worktree list --porcelain` entry reduced to what
// bears on identity: where the working tree is, what it has checked out, and the
// two ways git says a record has no working tree to hold a claim.
type worktreeRecord struct {
	path     string
	branch   string
	prunable bool
	bare     bool
}

// uninhabited reports that this record describes no working tree, which is the
// one reason to leave a record out of the enumeration.
//
// The two ways are different facts that happen to share an outcome. `prunable`
// is a working tree git has determined is gone — the deleted checkout this whole
// leg exists to catch. `bare` is a repository that never had one; lit cannot
// even resolve a workspace from a bare repository (Resolve's --show-toplevel
// fails there), so it mints no identity and produces no evidence.
func (w worktreeRecord) uninhabited() bool { return w.prunable || w.bare }

// parseWorktreeList reads `git worktree list --porcelain -z`. Records are groups
// of NUL-terminated `key value` fields, a record ends with an extra NUL — which
// arrives here as an empty field — and every record opens with a `worktree`
// field.
//
// [LAW:parse-dont-validate] Loose text goes in and worktreeRecords come out, so
// nothing downstream re-inspects git's output; the one place that understands
// this format is here.
//
// The accepted shapes, in full, because a format read loosely is a format read
// wrongly:
//
//	worktree <path>       opens a record; the path, verbatim
//	branch <ref>          the checked-out ref, rendered short
//	detached              no branch — a real state, so Branch is left empty
//	bare                  a repository with no working tree
//	prunable [<reason>]   git has determined the working tree is gone
//	locked [<reason>]     deliberately ignored: git already withholds `prunable`
//	                      from a locked record, so honoring it would be voting a
//	                      second time on a question git has answered
//	HEAD <sha>, anything  ignored — git has grown record attributes over
//	  else                releases (`locked` and `prunable` are both younger than
//	                      the format), and an unrecognized one must not break an
//	                      enumeration whose answer is about deleted worktrees
//
// An attribute field before any `worktree` field is refused rather than attached
// to nothing: it means the output is not the format this reads, and continuing
// would enumerate a repository's worktrees from something else entirely.
//
// [LAW:no-silent-failure] `-z` is what makes any of the above true, and it is
// not a nicety. A newline is a legal byte in a POSIX path, and the newline-
// terminated format emits paths raw, so a worktree at a path ending in the
// literal bytes "\nprunable" produced a record whose second LINE was the word
// `prunable` — parsed as an attribute of the live record it had just opened,
// which then silently vanished from the enumeration and took its checkout's
// claims with it. `\nbare` did the same; `\nbranch <ref>` corrupted the address.
// That is the precise failure this leg exists to prevent, reached without any
// error at all, and it was live here until a reviewer constructed it.
//
// The remedy is not a check. The newline-terminated format is ambiguous BY
// CONSTRUCTION for these paths — that is why git grew `-z` — so a parser reading
// it is guessing no matter how carefully it is written, and a heuristic that
// refuses fields "that might be a continuation" makes the guess safer without
// making it a parse. Under `-z` the path is one field with its newline intact
// and the ambiguity does not exist to be detected. [LAW:parse-dont-validate]
func parseWorktreeList(output string) ([]worktreeRecord, error) {
	var records []worktreeRecord
	for _, field := range strings.Split(output, "\x00") {
		// Cut on the FIRST space only: a worktree path may contain spaces — and,
		// now that the field is NUL-terminated, newlines — while every documented
		// key is a bare word, so the first space is always the boundary and the
		// rest of the field is the value byte for byte.
		key, value, _ := strings.Cut(field, " ")
		if key == "worktree" {
			records = append(records, worktreeRecord{path: value})
			continue
		}
		if key == "" {
			continue
		}
		if len(records) == 0 {
			return nil, fmt.Errorf("git worktree list --porcelain -z opened with %q, which is not a `worktree <path>` field", field)
		}
		current := &records[len(records)-1]
		switch key {
		case "branch":
			current.branch = strings.TrimPrefix(value, "refs/heads/")
		case "prunable":
			current.prunable = true
		case "bare":
			current.bare = true
		}
	}
	if len(records) == 0 {
		// Git lists the current worktree from inside any repository, so an empty
		// answer is not "no worktrees" — it is an answer that did not come from
		// the command this believes it ran. Reporting zero live checkouts from it
		// would void every claim this workspace holds. [LAW:no-silent-failure]
		return nil, fmt.Errorf("git worktree list --porcelain -z named no worktrees at all")
	}
	return records, nil
}
