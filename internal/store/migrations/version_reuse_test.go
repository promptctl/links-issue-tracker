package migrations

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// pinnedMigration is the released identity of one migration version: the file it
// shipped as and the sha256 of its exact bytes at release. The pin is a durable,
// in-repo snapshot of what git history assigned to a version number — the same
// choice baseline_frozen_test.go makes for v1 (pin the identity, not a commit
// hash a squash could rewrite, and not a live `git log` a shallow CI clone lacks).
type pinnedMigration struct {
	file   string
	sha256 string
}

// pinnedVersionContent maps every RELEASED, non-baseline migration version to its
// frozen identity. It is the single source of truth for "what content each version
// number has been assigned"; the guard below refuses any on-disk registry that
// diverges from it.
//
// Version 1 (the baseline) is deliberately absent — baseline_frozen_test.go is its
// content enforcer.
// [LAW:single-enforcer] one content enforcer per version number, never two: a
// second pin of v1 here would be a copy of the baseline hash, free to drift from it.
// [LAW:one-source-of-truth] adding a migration adds exactly one line here, in the
// same PR that adds the file; the pin and the file land together or the guard fails.
var pinnedVersionContent = map[int64]pinnedMigration{
	2: {file: "00002_add_lane.sql", sha256: "9126cd9e9c3a01137898fb9023c50b3f8741950f1e5d943dad521852939a4b75"},
	3: {file: "00003_add_resolution.sql", sha256: "47b50dea1a1e2b7e31ba6b0fa95f5f77c6411b81ee525b04e1ff5e5fdb469563"},
	4: {file: "00004_add_redirect_target.sql", sha256: "e171b9a18f13d67967e6c235499fc35254125ac23b36f9a687eddc7c411b590d"},
	5: {file: "00005_add_event_attribution.sql", sha256: "ed625b2817365ed357ad477cd0691994a93a65a7ab1005d1b05456c39d159a70"},
}

// migrationFile is one on-disk, non-baseline registry entry reduced to the identity
// this guard reasons about: its version number, its filename, and the sha256 of its
// exact bytes.
type migrationFile struct {
	version int64
	name    string
	sha256  string
}

// reuseKind enumerates the exhaustive ways the on-disk non-baseline registry can
// diverge from pinnedVersionContent.
//
// [LAW:types-are-the-program] the guard's output is a closed set of break shapes,
// not a free-form string, so the formatter must handle every one by structure and
// none can be silently dropped (TestEveryReuseKindHasMessage pins that).
type reuseKind int

const (
	kindContentChanged reuseKind = iota // a pinned version's bytes no longer match its pin
	kindRenamed                         // a pinned version's content matches but its filename changed
	kindUnpinned                        // an on-disk non-baseline version has no pin
	kindDeleted                         // a pinned version is gone from disk
	kindDuplicate                       // two on-disk files claim the same version
)

// reuseFinding is one detected divergence. pinned carries the released identity
// (set for kindContentChanged, kindRenamed, and kindDeleted); onDisk carries the
// current file(s) at that version (one for kindContentChanged/kindRenamed/
// kindUnpinned, two-or-more for kindDuplicate, none for kindDeleted).
type reuseFinding struct {
	kind    reuseKind
	version int64
	pinned  pinnedMigration
	onDisk  []migrationFile
}

// detectVersionReuse reports every way onDisk diverges from the pinned identities.
// It is pure — no filesystem, no globals — so its accept/reject behavior is pinned
// against synthetic fixtures in TestDetectVersionReuse.
//
// [LAW:effects-at-boundaries] the FS read and the hashing live in the test edge;
// this core is a total function from (pins, files) to findings.
// [LAW:dataflow-not-control-flow] a clean registry flows through the same
// enumeration as a broken one and simply yields an empty finding set.
func detectVersionReuse(pinned map[int64]pinnedMigration, onDisk []migrationFile) []reuseFinding {
	byVersion := map[int64][]migrationFile{}
	for _, f := range onDisk {
		byVersion[f.version] = append(byVersion[f.version], f)
	}

	var findings []reuseFinding
	for version, files := range byVersion {
		if len(files) > 1 {
			// Two files sharing a version number is a reuse-in-tree; it is the
			// overriding fault, so we don't also content-check the pair.
			findings = append(findings, reuseFinding{kind: kindDuplicate, version: version, onDisk: files})
			continue
		}
		f := files[0]
		pin, ok := pinned[version]
		if !ok {
			findings = append(findings, reuseFinding{kind: kindUnpinned, version: version, onDisk: files})
			continue
		}
		if f.sha256 != pin.sha256 {
			// Content drift is the dangerous, workspace-bricking case; it takes
			// priority over a filename difference on the same version.
			findings = append(findings, reuseFinding{kind: kindContentChanged, version: version, pinned: pin, onDisk: files})
			continue
		}
		if f.name != pin.file {
			// [LAW:one-source-of-truth] the pinned filename is authoritative, not a
			// decorative copy: enforcing it keeps pin.file from silently drifting
			// from the on-disk name (which explain() reports).
			findings = append(findings, reuseFinding{kind: kindRenamed, version: version, pinned: pin, onDisk: files})
		}
	}
	for version, pin := range pinned {
		if _, present := byVersion[version]; !present {
			findings = append(findings, reuseFinding{kind: kindDeleted, version: version, pinned: pin})
		}
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].version != findings[j].version {
			return findings[i].version < findings[j].version
		}
		return findings[i].kind < findings[j].kind
	})
	return findings
}

// explain renders a finding into an actionable failure message that names the
// collision and — crucially — steers the reader away from "just update the pin",
// which would re-enable the very bug class the gate exists to prevent.
func (f reuseFinding) explain() string {
	switch f.kind {
	case kindContentChanged:
		got := f.onDisk[0]
		return fmt.Sprintf(`version %d has been REUSED under different content.
  released as: %s (sha256 %s)
  now on disk: %s (sha256 %s)

goose keys migrations by version NUMBER, not by content. Every workspace that already
applied v%d is stamped on it and will NEVER re-run it, so the new content silently
never reaches those workspaces (missing columns, absent tables) — exactly the
migrate-drift epic's damage.

DO NOT change this version's pin in pinnedVersionContent to match the new bytes; that
re-enables the bug class this gate exists to prevent. Instead: keep v%d exactly as it
shipped, and add your change as the next free version number (a new NNNNN_*.sql),
pinned here in the same PR.`,
			f.version, f.pinned.file, f.pinned.sha256, got.name, got.sha256, f.version, f.version)
	case kindRenamed:
		got := f.onDisk[0]
		return fmt.Sprintf(`version %d kept its content but was RENAMED: %s -> %s.

A released migration's filename is part of its frozen identity; renaming it obscures
the registry's git history and leaves the pin's recorded filename stale. Restore the
name %s. If the rename is deliberate, update this version's %q field in
pinnedVersionContent in the same PR.`,
			f.version, f.pinned.file, got.name, f.pinned.file, "file")
	case kindUnpinned:
		got := f.onDisk[0]
		return fmt.Sprintf(`version %d (%s) is not pinned in pinnedVersionContent.

Every released, non-baseline migration must be pinned so this gate can refuse a later
reuse of its number under different content. If you just added this migration, add:
  %d: {file: %q, sha256: %q},
to pinnedVersionContent, in the same PR as the file.`,
			f.version, got.name, f.version, got.name, got.sha256)
	case kindDeleted:
		return fmt.Sprintf(`version %d was released as %s (sha256 %s) but is now MISSING from the registry.

A released migration must never be deleted: workspaces are stamped on its number, and
freeing that number invites a future file to reuse it under different content. Restore
the file. Retiring a version number is a schema decision that must not happen via a
silent deletion — raise it explicitly.`,
			f.version, f.pinned.file, f.pinned.sha256)
	case kindDuplicate:
		names := make([]string, len(f.onDisk))
		for i, m := range f.onDisk {
			names[i] = m.name
		}
		sort.Strings(names)
		return fmt.Sprintf(`version %d is claimed by %d files: %s.

Two files with the same leading version number are the same reuse collision by another
route — goose applies only one and the other's content is silently lost. Renumber all
but one to the next free version(s).`,
			f.version, len(f.onDisk), strings.Join(names, ", "))
	default:
		return fmt.Sprintf("unhandled reuse finding kind %d for version %d — add a message in reuseFinding.explain", f.kind, f.version)
	}
}

// TestReleasedMigrationsAreContentPinned refuses any change that reuses a released
// version number under different content — the exact mechanism that bricked
// workspaces in the migrate-drift epic, where a baseline rewrite refilled version
// slots 2/3 with new content while already-stamped workspaces kept the deleted
// content, so goose skipped the new columns forever.
//
// [LAW:single-enforcer] the one static gate on version-number reuse for v2+; v1 is
// enforced by baseline_frozen_test.go.
// [LAW:no-silent-failure] every divergence between the pins and the on-disk registry
// stops the build with a message naming the collision, so the drift can never reach a
// workspace unnoticed.
func TestReleasedMigrationsAreContentPinned(t *testing.T) {
	entries, err := FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read embedded registry: %v", err)
	}
	var onDisk []migrationFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, ok := ParseVersion(e.Name())
		if !ok {
			t.Fatalf("registry file %q does not begin with a numeric version", e.Name())
		}
		if v == Baseline {
			// [LAW:single-enforcer] v1's content is pinned by baseline_frozen_test.go.
			continue
		}
		data, err := FS.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("read %q: %v", e.Name(), err)
		}
		sum := sha256.Sum256(data)
		onDisk = append(onDisk, migrationFile{version: v, name: e.Name(), sha256: hex.EncodeToString(sum[:])})
	}
	for _, finding := range detectVersionReuse(pinnedVersionContent, onDisk) {
		t.Errorf("%s", finding.explain())
	}
}

// TestDetectVersionReuse pins the accept/reject table of the pure detector. A gate
// is only trustworthy if its rejection set is exactly the shapes it must refuse;
// these fixtures prove it fires on every reuse route and stays silent on a clean
// registry (including a legitimately appended, pinned migration).
//
// [LAW:behavior-not-structure] the fixtures assert WHICH collisions are detected
// (kind + version), not the wording of any message.
func TestDetectVersionReuse(t *testing.T) {
	pin := func(file, sha string) pinnedMigration { return pinnedMigration{file: file, sha256: sha} }
	file := func(v int64, name, sha string) migrationFile {
		return migrationFile{version: v, name: name, sha256: sha}
	}
	base := map[int64]pinnedMigration{
		2: pin("00002_add_lane.sql", "aaa"),
		3: pin("00003_add_resolution.sql", "bbb"),
		4: pin("00004_add_redirect_target.sql", "ccc"),
	}
	clean := []migrationFile{
		file(2, "00002_add_lane.sql", "aaa"),
		file(3, "00003_add_resolution.sql", "bbb"),
		file(4, "00004_add_redirect_target.sql", "ccc"),
	}

	cases := []struct {
		name   string
		pinned map[int64]pinnedMigration
		onDisk []migrationFile
		want   []reuseFinding // compared by kind + version only
	}{
		{
			name:   "clean — every pinned version present with matching content",
			pinned: base,
			onDisk: clean,
			want:   nil,
		},
		{
			name:   "content reuse — v2 refilled with different bytes",
			pinned: base,
			onDisk: []migrationFile{file(2, "00002_add_lane.sql", "DIFFERENT"), file(3, "00003_add_resolution.sql", "bbb"), file(4, "00004_add_redirect_target.sql", "ccc")},
			want:   []reuseFinding{{kind: kindContentChanged, version: 2}},
		},
		{
			name:   "content reuse under a renamed file — content drift wins over rename",
			pinned: base,
			onDisk: []migrationFile{file(2, "00002_add_lane.sql", "aaa"), file(3, "00003_renamed.sql", "DIFFERENT"), file(4, "00004_add_redirect_target.sql", "ccc")},
			want:   []reuseFinding{{kind: kindContentChanged, version: 3}},
		},
		{
			name:   "pure rename — same content, different filename",
			pinned: base,
			onDisk: []migrationFile{file(2, "00002_add_lane.sql", "aaa"), file(3, "00003_renamed.sql", "bbb"), file(4, "00004_add_redirect_target.sql", "ccc")},
			want:   []reuseFinding{{kind: kindRenamed, version: 3}},
		},
		{
			name:   "unpinned new migration — v5 present without a pin",
			pinned: base,
			onDisk: append(append([]migrationFile{}, clean...), file(5, "00005_new.sql", "eee")),
			want:   []reuseFinding{{kind: kindUnpinned, version: 5}},
		},
		{
			name:   "clean append — v5 present AND pinned",
			pinned: map[int64]pinnedMigration{2: pin("00002_add_lane.sql", "aaa"), 3: pin("00003_add_resolution.sql", "bbb"), 4: pin("00004_add_redirect_target.sql", "ccc"), 5: pin("00005_new.sql", "eee")},
			onDisk: append(append([]migrationFile{}, clean...), file(5, "00005_new.sql", "eee")),
			want:   nil,
		},
		{
			name:   "deleted — pinned v4 gone from disk",
			pinned: base,
			onDisk: []migrationFile{file(2, "00002_add_lane.sql", "aaa"), file(3, "00003_add_resolution.sql", "bbb")},
			want:   []reuseFinding{{kind: kindDeleted, version: 4}},
		},
		{
			name:   "duplicate — two files claim v2",
			pinned: base,
			onDisk: []migrationFile{file(2, "00002_add_lane.sql", "aaa"), file(2, "00002_add_lane_again.sql", "zzz"), file(3, "00003_add_resolution.sql", "bbb"), file(4, "00004_add_redirect_target.sql", "ccc")},
			want:   []reuseFinding{{kind: kindDuplicate, version: 2}},
		},
		{
			name:   "combined — a reuse and a deletion reported together, version-ordered",
			pinned: base,
			onDisk: []migrationFile{file(2, "00002_add_lane.sql", "DIFFERENT"), file(3, "00003_add_resolution.sql", "bbb")},
			want:   []reuseFinding{{kind: kindContentChanged, version: 2}, {kind: kindDeleted, version: 4}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectVersionReuse(tc.pinned, tc.onDisk)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d findings, want %d\n got: %+v\nwant: %+v", len(got), len(tc.want), got, tc.want)
			}
			for i := range got {
				if got[i].kind != tc.want[i].kind || got[i].version != tc.want[i].version {
					t.Errorf("finding[%d] = {kind:%d version:%d}, want {kind:%d version:%d}",
						i, got[i].kind, got[i].version, tc.want[i].kind, tc.want[i].version)
				}
			}
		})
	}
}

// TestEveryReuseKindHasMessage guarantees the formatter stays exhaustive: every
// declared reuseKind renders a real message that names the version, never the
// default "unhandled" fallthrough. A kind added without a message trips this.
//
// [LAW:no-silent-failure] a divergence the formatter can't describe is a divergence
// a reviewer can't act on; this keeps that unrepresentable.
func TestEveryReuseKindHasMessage(t *testing.T) {
	pin := pinnedMigration{file: "00007_x.sql", sha256: "cafef00d"}
	one := []migrationFile{{version: 7, name: "00007_x.sql", sha256: "deadbeef"}}
	two := []migrationFile{{version: 7, name: "00007_x.sql", sha256: "deadbeef"}, {version: 7, name: "00007_y.sql", sha256: "beefcafe"}}

	// Each fixture mirrors exactly what detectVersionReuse produces for that kind —
	// so explain() is exercised against the real onDisk/pinned shapes, and a future
	// explain() that over-indexes (onDisk[0] on a deleted finding, onDisk[1] on a
	// single-file finding) panics HERE instead of only in production.
	cases := []struct {
		kind    reuseKind
		finding reuseFinding
	}{
		{kindContentChanged, reuseFinding{kind: kindContentChanged, version: 7, pinned: pin, onDisk: one}},
		{kindRenamed, reuseFinding{kind: kindRenamed, version: 7, pinned: pin, onDisk: one}},
		{kindUnpinned, reuseFinding{kind: kindUnpinned, version: 7, onDisk: one}},
		{kindDeleted, reuseFinding{kind: kindDeleted, version: 7, pinned: pin}},
		{kindDuplicate, reuseFinding{kind: kindDuplicate, version: 7, onDisk: two}},
	}
	for _, tc := range cases {
		msg := tc.finding.explain()
		if strings.Contains(msg, "unhandled reuse finding kind") {
			t.Errorf("reuseKind %d has no dedicated message in explain()", tc.kind)
		}
		if !strings.Contains(msg, "7") {
			t.Errorf("reuseKind %d message does not name the version: %q", tc.kind, msg)
		}
	}
}
