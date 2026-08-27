package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// This file is the seam's enforcement. links-store-seam-q35v.4 flipped app and
// CLI onto lit's storage contract; what makes that flip durable is not the diff
// but the two checks below, which fail the build the moment it starts to heal
// over. [LAW:verifiable-goals]
//
// The line they draw is the ENGINE HANDLE. No package above internal/store may
// name the concrete engine type or construct one: engines arrive as
// [storage.Store] from internal/engine, and anything beyond the core contract
// is asked for with storage.<Cap>.Of. That is what the campaign presses on —
// S1 decorates what the factory returns, the oracle diffs through the contract,
// S2 changes which engine is behind it — and a single `*store.Store` in a CLI
// signature would put all of it back out of reach.
//
// What the CLI still names in the engine package is Dolt-era workspace
// machinery addressed by filesystem path rather than by engine handle, and it
// is enumerated rather than banned. See doltWorkspaceMachinery below for why
// enumerating is the honest answer and what makes the list shrink.

const engineImportPath = "github.com/promptctl/links-issue-tracker/internal/store"

// engineHandles are the names that mean "a concrete Dolt engine" — the type
// itself and every constructor that mints one.
//
// These are the symbols whose absence IS the seam. Everything else the engine
// package exports can be argued about; these cannot, because naming one is how
// a caller comes to depend on the engine's identity rather than on what lit
// needs from storage. [LAW:one-way-deps]
var engineHandles = []string{"Store", "Open", "OpenForRead", "OpenSync"}

// doltWorkspaceMachinery is every remaining engine-package symbol app, CLI, and
// cmd/lit may name, grouped by what it is.
//
// None of it is a storage contract. Every entry takes a filesystem path, not an
// engine handle, and most of it runs when no engine is open or can be opened:
// locks taken before construction, beacons between processes, bootstrap that
// creates the database, recovery for a workspace the engine refuses.
//
// It is enumerated rather than wrapped because
// design-docs/event-store/design.md §migration schedules exactly this machinery
// for DELETION at S4 — "reconcile, engine-serialization, mirror-flock machinery
// are deleted" — not for a second implementation. An interface carved over code
// with no second implementer coming is carrying cost with no payer
// [LAW:carrying-cost], and a forwarding package that merely re-exported these
// names would make the import ban technically true and architecturally
// meaningless — the very alias-shell that links-store-seam-q35v.1 called
// scaffolding with a demolition date.
//
// So the list is a ratchet instead. Reaching for a new engine symbol fails the
// build, and an entry that no caller uses any more also fails the build, which
// is what makes this shrink monotonically toward empty as the campaign
// proceeds. It is the honest map of where the seam actually is.
var doltWorkspaceMachinery = map[string][]string{
	// Kernel flocks and the paths they guard: taken around snapshot restore and
	// commit, before and after any engine exists.
	"workspace and commit locks": {
		"CommitLockPath", "LockCommitPath", "SettleCommitLockRelease",
		"LockWorkspaceExclusive", "LockWorkspaceShared", "LockDoltJournalExclusive",
		"TryAcquireSyncPushLock",
	},
	// The detached mirror's cross-process handshake. Named in design.md §migration
	// as mirror-flock machinery, deleted at S4.
	"mirror beacons": {
		"MirrorBeaconLockPath", "ProbeMirrorBeacon", "HoldMirrorBeacon",
		"BeaconAnswered", "BeaconUnheld",
	},
	// Creating a workspace and adopting a remote's history — all of it runs
	// before there is an engine to ask.
	"bootstrap and remote adoption": {
		"EnsureDatabase", "LocalHasTickets", "GitBackedRemoteURL",
		"AdoptRemoteByClone", "PendingAdopt", "AdoptPendingMarkerPath",
	},
	// Reading a snapshot's provenance out of its name, for the snapshots listing.
	"snapshot naming": {
		"IsMigrationSnapshotName", "IsDowngradeSnapshotName", "IsReconcileSnapshotName",
	},
	// Lifeboat: raw-table recovery for a workspace the engine will not open, so
	// by definition it cannot go through the contract.
	"lifeboat recovery": {
		"RawDump", "DumpRaw", "Recover", "HealWorkspace", "PromoteCandidate",
		"ShapeMapping", "Mapper", "DeterministicMapper", "DeterministicMap",
		"ColumnRef", "FromColumn",
		"Reconciled", "Unconverged", "UnexplainedDrop", "RequiresDrop",
	},
	// Typed failures the CLI matches to choose an exit code and a message.
	// [LAW:parse-dont-validate] — matched as types, never by message text.
	"typed engine failures": {
		"ErrWorkspaceBusy", "ErrTransientGCContention", "WorkspaceWriteBlockedError",
		"UnsupportedSchemaVersionError", "RemoteSchemaAheadError",
		"OwnerApprovalRequiredError",
	},
}

// seamPackages are the packages that must reach storage through the contract:
// everything lit's commands are built from, relative to this file's directory.
var seamPackages = []string{".", "../app", "../../cmd/lit"}

// TestCLIAndAppReachStorageOnlyThroughTheContract is the ticket's lasting
// artifact: it makes the seam unrepresentable to cross rather than merely
// discouraged.
//
// It reads source rather than trusting a convention, and it covers test files
// too — a fixture that opens a concrete engine is exactly how the boundary
// erodes first, and the in-memory engine from links-store-seam-q35v.3 means a
// test needing storage behavior no longer needs the Dolt one.
func TestCLIAndAppReachStorageOnlyThroughTheContract(t *testing.T) {
	t.Parallel()

	allowed := map[string]string{}
	for family, names := range doltWorkspaceMachinery {
		for _, name := range names {
			if prior, dup := allowed[name]; dup {
				t.Fatalf("%q is listed under both %q and %q; one symbol, one home", name, prior, family)
			}
			allowed[name] = family
		}
	}

	used := enginePackageSymbolsUsedBy(t, seamPackages)

	// The seam itself: no concrete engine, anywhere above internal/store.
	for _, handle := range engineHandles {
		if where, named := used[handle]; named {
			t.Errorf(
				"%s names the concrete Dolt engine as store.%s (%s).\n"+
					"Engines reach app and CLI as storage.Store from internal/engine, and\n"+
					"capabilities beyond it come from storage.<Cap>.Of — never from the\n"+
					"engine's own type. See the package comment above.",
				strings.Join(where, ", "), handle, engineImportPath,
			)
		}
	}

	// Additions: something new was reached for in the engine package.
	for name, where := range used {
		if _, ok := allowed[name]; ok || slices.Contains(engineHandles, name) {
			continue
		}
		t.Errorf(
			"%s reaches for store.%s, which the seam does not allow.\n"+
				"If lit needs it from storage, it belongs in internal/storage — as a\n"+
				"contract method or a capability. If it is Dolt-era workspace machinery\n"+
				"like the entries in doltWorkspaceMachinery, add it there WITH its family\n"+
				"and the reason it cannot go through the contract.",
			strings.Join(where, ", "), name,
		)
	}

	// Removals: the ratchet. An allowance nobody uses has to go, or the list
	// stops describing the residue and starts excusing it.
	for name, family := range allowed {
		if _, ok := used[name]; !ok {
			t.Errorf(
				"doltWorkspaceMachinery allows store.%s under %q, but nothing reaches for it any more.\n"+
					"Delete the entry: this list only shrinks, and a stale allowance is a\n"+
					"door left open for the next caller.",
				name, family,
			)
		}
	}
}

// enginePackageSymbolsUsedBy reports every symbol the given package directories
// name in the engine package, mapped to the files that name it.
//
// It resolves the import's local name per file rather than assuming "store", so
// an aliased import cannot slip past, and it skips selectors whose base is a
// local variable that merely shares the name.
func enginePackageSymbolsUsedBy(t *testing.T, dirs []string) map[string][]string {
	t.Helper()
	used := map[string][]string{}
	for _, dir := range dirs {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", dir, err)
		}
		for _, pkg := range pkgs {
			for path, file := range pkg.Files {
				local, imports := engineImportName(file)
				if !imports {
					continue
				}
				where := filepath.Join(filepath.Base(dir), filepath.Base(path))
				ast.Inspect(file, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					base, ok := sel.X.(*ast.Ident)
					// A non-nil Obj means the base resolved to a local
					// declaration, so this is a field or method on a value that
					// happens to share the import's name, not a package symbol.
					if !ok || base.Name != local || base.Obj != nil {
						return true
					}
					if !slices.Contains(used[sel.Sel.Name], where) {
						used[sel.Sel.Name] = append(used[sel.Sel.Name], where)
					}
					return true
				})
			}
		}
	}
	for name := range used {
		sort.Strings(used[name])
	}
	return used
}

// engineImportName returns the name the file refers to the engine package by,
// and whether it imports it at all.
func engineImportName(file *ast.File) (string, bool) {
	for _, spec := range file.Imports {
		if strings.Trim(spec.Path.Value, `"`) != engineImportPath {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name, true
		}
		return "store", true
	}
	return "", false
}
