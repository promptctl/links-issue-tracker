package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dolthub/dolt/go/cmd/dolt/cli"
	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/store/chunks"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// Compaction depth, and the on-disk evidence for when each depth is due.
//
// Dolt collects in two depths and lit historically pinned the shallower one by
// calling DOLT_GC with no arguments, which made the deeper one unreachable from
// anywhere in this codebase. They are not interchangeable and neither reclaims
// what the other does: measured on this repository's own store, the shallow
// depth freed zero bytes while the deep one freed 12 MB and collapsed 148
// archive files into 2; measured on a never-pushed workspace, the shallow depth
// freed 69% and the deep one added nothing on top. Carrying the depth as a
// value is what lets one implementation serve both.
// [LAW:dataflow-not-control-flow]

// GCMode re-exports the contract's depth vocabulary under the engine's own
// name, matching how this package already re-exports the rest of the sync
// types. There is one declaration; this is an alias, not a second enum.
// [LAW:one-source-of-truth]
type GCMode = storage.GCMode

const (
	GCNewGen = storage.GCNewGen
	GCFull   = storage.GCFull
)

// gcProcedureArgs renders a depth as DOLT_GC's own arguments. The flag spelling
// comes from the constant Dolt's argument parser reads, so a rename breaks the
// build here instead of silently selecting the wrong depth. It lives in this
// package rather than beside the enum because how a depth is *spelled* is
// engine knowledge, while the depth itself is contract vocabulary.
// [LAW:one-source-of-truth]
//
// A GCMode outside the enum can only arrive through a cast, and the honest
// answer to one is a refusal: selecting a depth by falling through to the
// shallower default is precisely the silent wrong-work this type exists to
// prevent. [LAW:no-silent-failure]
func gcProcedureArgs(m GCMode) ([]string, error) {
	switch m {
	case GCNewGen:
		return nil, nil
	case GCFull:
		return []string{"--" + cli.FullFlag}, nil
	}
	return nil, fmt.Errorf("unknown compaction mode %d", int(m))
}

const (
	// journalDueBytes is when the new generation has grown enough to be worth a
	// shallow pass. Dolt's own auto-GC thresholds the same signal at 128 MB
	// (sqle/auto_gc.go), sized for a long-lived server holding databases orders
	// of magnitude larger than an issue tracker's; 16 MB keeps the same policy
	// shape at this domain's scale. It was chosen against measurement, not
	// taste: a shallow pass over a 5.9 MB journal took 0.26s, so this bounds
	// the stall it can impose below a second, arrives roughly every 350
	// mutations, and caps undisturbed journal waste at ~11 MB.
	journalDueBytes int64 = 16 << 20

	// archivesDueCount is when the old generation has fragmented enough to be
	// worth a deep pass. Each shallow pass appends one archive and removes
	// none, so this counts passes since the last deep collection rather than
	// estimating waste. A deep pass collapses the count to a handful, so the
	// counter resets hard rather than hovering at the threshold.
	archivesDueCount = 64

	// archiveFileExt is the suffix Dolt gives an old-generation archive.
	archiveFileExt = ".darc"

	// oldGenDirName is the old generation's directory inside the chunk store.
	// Dolt exports no constant for it, so it is named once, here.
	oldGenDirName = "oldgen"
)

// storeFootprint is the on-disk evidence the compaction policy decides on. It
// holds sizes only — no verdict — so the measurement and the policy can be
// tested apart, and so the policy stays a pure function of numbers.
type storeFootprint struct {
	// JournalBytes is the chunk journal's size: uncollected new-generation
	// history. Zero is a truthful reading of a store whose journal has just
	// been collected away, not a failure.
	JournalBytes int64
	// OldGenArchives counts archive files in the old generation.
	OldGenArchives int
}

// nomsDir is where a store rooted at doltRootDir keeps its chunks. Every
// segment comes from the constant that defines it, matching remoteCacheBase's
// derivation rather than spelling the layout a second time.
// [LAW:one-source-of-truth]
func nomsDir(doltRootDir string) string {
	return filepath.Join(doltRootDir, doltDatabaseName, dbfactory.DoltDir, dbfactory.DataDir)
}

// measureFootprint reads the two sizes the compaction policy runs on. This is
// the whole effect: one Stat and one directory read, with no engine open and no
// lock held, which is what lets a cadence owner ask "is compaction due" on an
// ordinary command without paying an engine open for the answer.
// [LAW:effects-at-boundaries]
//
// It takes a path rather than an open store precisely so the question can be
// asked before deciding to open one — the cheap probe is the entire point of
// the threshold.
//
// A store directory that does not exist yet reads as an empty footprint — that
// is the true size of a store with nothing in it. Any other failure is
// returned: "I could not measure" and "there is nothing to collect" are
// different facts, and a policy fed the second when the first is true would
// skip maintenance forever on an unreadable store. [LAW:no-silent-failure]
func measureFootprint(doltRootDir string) (storeFootprint, error) {
	noms := nomsDir(doltRootDir)
	footprint := storeFootprint{}

	// The journal's filename is Dolt's own exported constant, so this reads the
	// real file rather than a guessed one. [LAW:one-source-of-truth]
	info, err := os.Stat(filepath.Join(noms, chunks.JournalFileID))
	switch {
	case err == nil:
		footprint.JournalBytes = info.Size()
	case !os.IsNotExist(err):
		return storeFootprint{}, fmt.Errorf("measure chunk journal: %w", err)
	}

	entries, err := os.ReadDir(filepath.Join(noms, oldGenDirName))
	switch {
	case err == nil:
		for _, entry := range entries {
			if !entry.IsDir() && filepath.Ext(entry.Name()) == archiveFileExt {
				footprint.OldGenArchives++
			}
		}
	case !os.IsNotExist(err):
		return storeFootprint{}, fmt.Errorf("measure old generation: %w", err)
	}

	return footprint, nil
}

// measureFootprint measures the store this handle is open on.
func (s *Store) measureFootprint() (storeFootprint, error) {
	return measureFootprint(s.doltRootDir)
}

// dueMode reports which compaction depth this footprint warrants, if any.
// Pure — no clock, no filesystem, no store — so the policy is exercised by
// handing it numbers. [LAW:effects-at-boundaries]
//
// The deep depth is tested first because it subsumes the shallow one: a pass
// that rewrites the old generation also collects the new one, so a store that
// warrants both wants a single deep pass rather than two.
//
// The comma-ok shape carries "nothing is due" rather than a third enum member,
// so there is one representation of compaction depth for both the policy and
// the store call. A second enum would have to be kept in step with this one,
// and the two could then disagree about what "full" means.
// [LAW:one-source-of-truth]
func dueMode(footprint storeFootprint) (GCMode, bool) {
	if footprint.OldGenArchives >= archivesDueCount {
		return GCFull, true
	}
	if footprint.JournalBytes >= journalDueBytes {
		return GCNewGen, true
	}
	return GCNewGen, false
}

// footprintDelta renders what a pass changed, in this engine's own vocabulary.
// Journals and generations are Dolt's words, so they are spoken here and handed
// across the contract already rendered — a caller that had to format them would
// need to understand a storage layout the contract exists to hide from it.
// [LAW:decomposition]
//
// A measurement that failed says so instead of reporting a delta computed from
// a number nobody read. [LAW:no-silent-failure]
func footprintDelta(before, after storeFootprint, measureErr error) string {
	if measureErr != nil {
		return fmt.Sprintf("footprint not measured: %v", measureErr)
	}
	return fmt.Sprintf("journal %s -> %s, old-generation archives %d -> %d",
		humanBytes(before.JournalBytes), humanBytes(after.JournalBytes),
		before.OldGenArchives, after.OldGenArchives)
}

// compactionReport says what the push path's compaction did, and says nothing
// about the shallow pass every push already runs — an operator does not need
// telling that the routine thing happened routinely. A deep pass is worth a
// line because it is rare and because it explains a push that took noticeably
// longer than usual.
//
// A failed measurement is reported even though the compaction still ran, and it
// names the depth that ran, so "deep collection is overdue and I cannot tell"
// never reads as "nothing needed doing". [LAW:no-silent-failure]
func compactionReport(mode GCMode, measureErr error) string {
	switch {
	case measureErr != nil:
		return fmt.Sprintf("compaction: ran %s pass; could not measure whether a deeper one is due: %v", mode, measureErr)
	case mode == GCFull:
		return "compaction: ran full pass, rewriting the old generation"
	default:
		return ""
	}
}

// joinMaintenance combines the maintenance lines that have something to say and
// drops the ones that do not, so a quiet run stays quiet and adds no output
// noise to an ordinary push.
func joinMaintenance(reports ...string) string {
	spoken := make([]string, 0, len(reports))
	for _, report := range reports {
		if report != "" {
			spoken = append(spoken, report)
		}
	}
	return strings.Join(spoken, "; ")
}
