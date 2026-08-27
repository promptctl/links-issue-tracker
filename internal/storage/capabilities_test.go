package storage_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// The engines below satisfy an interface by embedding it rather than
// implementing it: an embedded nil interface promotes the whole method set, so
// a fake declares what it offers in one line and panics only if a test calls a
// method it never claimed to have. That is the point — these tests are about
// what an engine ANSWERS when asked, and every capability's answer must be
// decidable without performing it. [LAW:behavior-not-structure]

type bareEngine struct{ storage.Store }

type syncEngine struct {
	storage.Store
	storage.Syncer
}

type reconcileEngine struct {
	storage.Store
	storage.Reconciler
}

type checkpointEngine struct {
	storage.Store
	storage.Checkpointer
}

type repairEngine struct {
	storage.Store
	storage.Repairer
}

type schemaEngine struct {
	storage.Store
	storage.SchemaMigrator
}

type importEngine struct {
	storage.Store
	storage.Importer
}

type rawEngine struct {
	storage.Store
	storage.RawExecutor
}

type fullEngine struct {
	storage.Store
	storage.Syncer
	storage.Reconciler
	storage.Checkpointer
	storage.Repairer
	storage.SchemaMigrator
	storage.Importer
	storage.RawExecutor
}

// capabilityCases pairs each capability with an engine offering that one and
// nothing else. Every case therefore proves two things at once: the capability
// is found on an engine that has it, and it is NOT found on the six engines
// that don't — which is what catches a capability whose name has been wired to
// the wrong interface.
func capabilityCases() []struct {
	capability storage.Capability
	engine     storage.Store
} {
	return []struct {
		capability storage.Capability
		engine     storage.Store
	}{
		{storage.Sync, syncEngine{}},
		{storage.Reconcile, reconcileEngine{}},
		{storage.Checkpoints, checkpointEngine{}},
		{storage.Repair, repairEngine{}},
		{storage.SchemaMigration, schemaEngine{}},
		{storage.Import, importEngine{}},
		{storage.TestSupport, rawEngine{}},
	}
}

func TestEngineOffersOnlyWhatItImplements(t *testing.T) {
	for _, c := range capabilityCases() {
		t.Run(c.capability.Name(), func(t *testing.T) {
			offered := storage.Offered(c.engine)
			if len(offered) != 1 || offered[0].Name() != c.capability.Name() {
				t.Fatalf("engine offering only %s reported %v", c.capability.Name(), names(offered))
			}
			for _, other := range storage.Capabilities() {
				want := other.Name() == c.capability.Name()
				if got := other.OfferedBy(c.engine); got != want {
					t.Errorf("%s.OfferedBy(%s engine) = %v, want %v",
						other.Name(), c.capability.Name(), got, want)
				}
			}
		})
	}
}

func TestAbsentCapabilityAnswersWithTypedAbsence(t *testing.T) {
	engine := bareEngine{}
	if offered := storage.Offered(engine); len(offered) != 0 {
		t.Fatalf("an engine implementing only the core contract offered %v", names(offered))
	}
	// Every capability must be askable of an engine that has none of them, and
	// every answer must be the same typed refusal — never a panic, and never a
	// nil the caller would have to know to check. [LAW:no-silent-failure]
	assertRefused := func(t *testing.T, capability string, err error) {
		t.Helper()
		var unsupported storage.UnsupportedError
		if !errors.As(err, &unsupported) {
			t.Fatalf("%s: got %v, want UnsupportedError", capability, err)
		}
		if unsupported.Capability != capability {
			t.Errorf("%s: error names capability %q", capability, unsupported.Capability)
		}
		if unsupported.Engine == "" {
			t.Errorf("%s: error does not name the engine that came up short", capability)
		}
	}

	_, err := storage.Sync.Of(engine)
	assertRefused(t, "sync", err)
	_, err = storage.Reconcile.Of(engine)
	assertRefused(t, "reconcile", err)
	_, err = storage.Checkpoints.Of(engine)
	assertRefused(t, "checkpoints", err)
	_, err = storage.Repair.Of(engine)
	assertRefused(t, "repair", err)
	_, err = storage.SchemaMigration.Of(engine)
	assertRefused(t, "schema-migration", err)
	_, err = storage.Import.Of(engine)
	assertRefused(t, "import", err)
	_, err = storage.TestSupport.Of(engine)
	assertRefused(t, "test-support", err)
}

// syncOnlyReconcileless is the shape the event store will take: it pushes,
// fetches, and reports where it stands, and it has no divergence to settle
// because its arrivals do not conflict (design-docs/event-store/design.md
// §sync). The contract has to let it say so — that is the whole reason
// reconcile is not folded into sync.
type syncOnlyReconcileless struct {
	storage.Store
	storage.Syncer
	compacted   bool
	compactedAt storage.GCMode
}

func (e *syncOnlyReconcileless) SyncCompact(_ context.Context, mode storage.GCMode) (storage.CompactionOutcome, error) {
	e.compacted = true
	e.compactedAt = mode
	return storage.CompactionOutcome{Ran: true, Depth: mode}, nil
}

func (e *syncOnlyReconcileless) CompactIfDue(context.Context) (storage.CompactionOutcome, error) {
	return storage.CompactionOutcome{}, nil
}

func TestSyncWithoutReconcileIsRepresentable(t *testing.T) {
	engine := &syncOnlyReconcileless{}

	syncer, err := storage.Sync.Of(engine)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := storage.Reconcile.Of(engine); err == nil {
		t.Fatal("an engine with no divergence to settle was reported as offering reconcile")
	}

	// Of hands back the engine itself, not a wrapper around it: calling
	// through the capability must reach the engine's own method.
	if _, err := syncer.SyncCompact(t.Context(), storage.GCFull); err != nil {
		t.Fatalf("SyncCompact through the capability: %v", err)
	}
	if !engine.compacted {
		t.Fatal("the call through the capability did not reach the engine")
	}
	// The depth is part of the contract, so it must survive the crossing —
	// a capability that reached the engine but dropped the requested depth
	// would silently collect at the wrong one.
	if engine.compactedAt != storage.GCFull {
		t.Fatalf("compaction depth through the capability = %v, want %v", engine.compactedAt, storage.GCFull)
	}
}

func TestOfferedFollowsCapabilitiesOrder(t *testing.T) {
	got := names(storage.Offered(fullEngine{}))
	want := names(storage.Capabilities())
	if !slices.Equal(got, want) {
		t.Fatalf("an engine offering everything reported %v, want %v", got, want)
	}
}

func TestCapabilitiesIsTheCallersOwnSlice(t *testing.T) {
	first := storage.Capabilities()
	before := names(first)
	slices.Reverse(first)

	if after := names(storage.Capabilities()); !slices.Equal(after, before) {
		t.Fatalf("mutating a returned slice changed the enumeration: %v, want %v", after, before)
	}
}

func TestCapabilityNamesAreDistinctAndSpoken(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range storage.Capabilities() {
		if c.Name() == "" {
			t.Error("a capability has no name; UnsupportedError would name nothing")
		}
		if seen[c.Name()] {
			t.Errorf("two capabilities answer to %q", c.Name())
		}
		seen[c.Name()] = true
	}
}

// TestEveryCapabilityIsEnumerated closes the one gap Go's type system leaves
// open here: a capability declared in the package but left out of the
// enumeration would be askable by name and invisible to every listing, and
// nothing in a build or a passing suite would say so. The declarations are the
// source of truth, so the check reads them.
func TestEveryCapabilityIsEnumerated(t *testing.T) {
	pkgs, err := parser.ParseDir(token.NewFileSet(), ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the package: %v", err)
	}

	declared := []string{}
	for _, pkg := range pkgs {
		ast.Inspect(pkg, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for _, value := range spec.Values {
				lit, ok := value.(*ast.CompositeLit)
				if !ok {
					continue
				}
				index, ok := lit.Type.(*ast.IndexExpr)
				if !ok {
					continue
				}
				if ident, ok := index.X.(*ast.Ident); !ok || ident.Name != "capability" {
					continue
				}
				declared = append(declared, capabilityLiteralName(t, lit))
			}
			return true
		})
	}

	slices.Sort(declared)
	enumerated := names(storage.Capabilities())
	slices.Sort(enumerated)
	if !slices.Equal(declared, enumerated) {
		t.Fatalf("declared capabilities %v, but Capabilities() reports %v", declared, enumerated)
	}
}

func capabilityLiteralName(t *testing.T, lit *ast.CompositeLit) string {
	t.Helper()
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "name" {
			continue
		}
		value, ok := kv.Value.(*ast.BasicLit)
		if !ok {
			break
		}
		unquoted, err := strconv.Unquote(value.Value)
		if err != nil {
			break
		}
		return unquoted
	}
	t.Fatalf("a capability is declared without a literal name: %#v", lit)
	return ""
}

func names(capabilities []storage.Capability) []string {
	out := make([]string, 0, len(capabilities))
	for _, c := range capabilities {
		out = append(out, c.Name())
	}
	return out
}

// GCMode is a closed set, and which values belong to it is a fact about this
// contract rather than about any engine — so the rejection lives on the type,
// where every engine reaches the same answer. Asserting both arms matters more
// than it looks: a Valid that only ever said true would pass a test that
// checked the legal depths alone, while admitting exactly the illegal ones it
// exists to stop. [LAW:behavior-not-structure]
func TestGCModeValidAcceptsOnlyTheContractsDepths(t *testing.T) {
	t.Parallel()

	for _, legal := range []storage.GCMode{storage.GCNewGen, storage.GCFull} {
		if !legal.Valid() {
			t.Fatalf("GCMode(%s).Valid() = false, want true — it is one of the contract's own depths", legal)
		}
	}
	for _, illegal := range []storage.GCMode{storage.GCMode(-1), storage.GCMode(2), storage.GCMode(99)} {
		if illegal.Valid() {
			t.Fatalf("GCMode(%d).Valid() = true, want false — an engine would collect at a depth nobody named", int(illegal))
		}
	}
}
