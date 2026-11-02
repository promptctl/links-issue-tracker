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
// shipped as and the sha256 of its Up section (upSection) at release. Only the Up
// is frozen: goose never re-runs a stamped version's Up, but a Down always runs
// from the binary doing the downgrade, so a released Down is fixed in place.
// The pin is a durable,
// in-repo snapshot of what git history assigned to a version number — the same
// choice baseline_frozen_test.go makes for v1 (pin the identity, not a commit
// hash a squash could rewrite, and not a live `git log` a shallow CI clone lacks).
type pinnedMigration struct {
	file     string
	upSHA256 string
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
	2:  {file: "00002_add_lane.sql", upSHA256: "b7f402fbc9356b075026f92bb95735e83607a47c0a65cf3537c66d19fdd2ca8a"},
	3:  {file: "00003_add_resolution.sql", upSHA256: "fc4950d6551b19a55c71c28b6a6d45da2d79f8b034a132f9433904954ba29a1b"},
	4:  {file: "00004_add_redirect_target.sql", upSHA256: "e05850fa88ee369a5c28e9d55f7a1c4f09ba8fa02617fc1913899da124b21aa3"},
	5:  {file: "00005_add_event_attribution.sql", upSHA256: "0b77eca74d253be6d473361c99e7f7c442afa1770eec84adce65cfe3638455a8"},
	6:  {file: "00006_add_follows_up.sql", upSHA256: "efa661fe146b028c3dc724b81e848e59603ffded4328271d17d5b3ac791bd1dd"},
	7:  {file: "00007_every_ticket_inside_a_container.sql", upSHA256: "309734ef37630b3231dd706f63c37858ffe132f651a28fbcc9f7ddafcb57815f"},
	8:  {file: "00008_blocked_by_edge.sql", upSHA256: "b692c89627a4a9d3149e2c3c073b7c3845e512891598c2fe947c0b9db6b80a59"},
	9:  {file: "00009_purges.sql", upSHA256: "c3aca522c2e2cada23b7d2b3e1e140b3fa5dc902430864f5b1f7d24a70021587"},
	10: {file: "00010_history_can_be_re_rooted.sql", upSHA256: "d9262bb984cc5c0a80d839beec6f940880c142539e74a909af28c2fdd12a03f4"},
	11: {file: "00011_plans_and_plan_reviews.sql", upSHA256: "8477d335ea8d62c52c94b44792477318e22fcdcfdddfbc6a623e29e9be2a85f7"},
}

// migrationFile is one on-disk, non-baseline registry entry reduced to the identity
// this guard reasons about: its version number, its filename, and the sha256 of its
// Up section.
type migrationFile struct {
	version  int64
	name     string
	upSHA256 string
}

// reuseKind enumerates the exhaustive ways the on-disk non-baseline registry can
// diverge from pinnedVersionContent.
//
// [LAW:types-are-the-program] the guard's output is a closed set of break shapes,
// not a free-form string, so the formatter must handle every one by structure and
// none can be silently dropped (TestEveryReuseKindHasMessage pins that).
type reuseKind int

const (
	kindContentChanged reuseKind = iota // a pinned version's Up section no longer matches its pin
	kindRenamed                         // a pinned version's content matches but its filename changed
	kindUnpinned                        // an on-disk non-baseline version has no pin
	kindDeleted                         // a pinned version is gone from disk
)

// reuseFinding is one detected divergence. pinned carries the released identity
// (set for kindContentChanged, kindRenamed, and kindDeleted); onDisk carries the
// current file at that version (set for kindContentChanged, kindRenamed, and
// kindUnpinned).
type reuseFinding struct {
	kind    reuseKind
	version int64
	pinned  pinnedMigration
	onDisk  migrationFile
}

// detectVersionReuse reports every way onDisk diverges from the pinned identities.
// onDisk holds one file per version: ParseRegistry refuses a version claimed
// twice. It is pure — no filesystem, no globals — so its accept/reject behavior is pinned
// against synthetic fixtures in TestDetectVersionReuse.
//
// [LAW:effects-at-boundaries] the FS read and the hashing live in the test edge;
// this core is a total function from (pins, files) to findings.
// [LAW:dataflow-not-control-flow] a clean registry flows through the same
// enumeration as a broken one and simply yields an empty finding set.
func detectVersionReuse(pinned map[int64]pinnedMigration, onDisk []migrationFile) []reuseFinding {
	byVersion := map[int64]migrationFile{}
	for _, f := range onDisk {
		byVersion[f.version] = f
	}

	var findings []reuseFinding
	for version, f := range byVersion {
		pin, ok := pinned[version]
		if !ok {
			findings = append(findings, reuseFinding{kind: kindUnpinned, version: version, onDisk: f})
			continue
		}
		if f.upSHA256 != pin.upSHA256 {
			// Content drift is the dangerous, workspace-bricking case; it takes
			// priority over a filename difference on the same version.
			findings = append(findings, reuseFinding{kind: kindContentChanged, version: version, pinned: pin, onDisk: f})
			continue
		}
		if f.name != pin.file {
			// [LAW:one-source-of-truth] the pinned filename is authoritative, not a
			// decorative copy: enforcing it keeps pin.file from silently drifting
			// from the on-disk name (which explain() reports).
			findings = append(findings, reuseFinding{kind: kindRenamed, version: version, pinned: pin, onDisk: f})
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
		got := f.onDisk
		return fmt.Sprintf(`version %d has been REUSED under different content.
  released as: %s (Up sha256 %s)
  now on disk: %s (Up sha256 %s)

goose keys migrations by version NUMBER, not by content. Every workspace that already
applied v%d is stamped on it and will NEVER re-run it, so the new content silently
never reaches those workspaces (missing columns, absent tables) — exactly the
migrate-drift epic's damage.

DO NOT change this version's pin in pinnedVersionContent to match the new bytes; that
re-enables the bug class this gate exists to prevent. Instead: keep v%d's Up exactly
as it shipped, and add your change as the next free version number (a new
NNNNN_*.sql), pinned here in the same PR. Its Down is not pinned and may be fixed in
place.`,
			f.version, f.pinned.file, f.pinned.upSHA256, got.name, got.upSHA256, f.version, f.version)
	case kindRenamed:
		got := f.onDisk
		return fmt.Sprintf(`version %d kept its content but was RENAMED: %s -> %s.

A released migration's filename is part of its frozen identity; renaming it obscures
the registry's git history and leaves the pin's recorded filename stale. Restore the
name %s. If the rename is deliberate, update this version's %q field in
pinnedVersionContent in the same PR.`,
			f.version, f.pinned.file, got.name, f.pinned.file, "file")
	case kindUnpinned:
		got := f.onDisk
		return fmt.Sprintf(`version %d (%s) is not pinned in pinnedVersionContent.

Every released, non-baseline migration must be pinned so this gate can refuse a later
reuse of its number under different content. If you just added this migration, add:
  %d: {file: %q, upSHA256: %q},
to pinnedVersionContent, in the same PR as the file.`,
			f.version, got.name, f.version, got.name, got.upSHA256)
	case kindDeleted:
		return fmt.Sprintf(`version %d was released as %s (Up sha256 %s) but is now MISSING from the registry.

A released migration must never be deleted: workspaces are stamped on its number, and
freeing that number invites a future file to reuse it under different content. Restore
the file. Retiring a version number is a schema decision that must not happen via a
silent deletion — raise it explicitly.`,
			f.version, f.pinned.file, f.pinned.upSHA256)
	default:
		return fmt.Sprintf("unhandled reuse finding kind %d for version %d — add a message in reuseFinding.explain", f.kind, f.version)
	}
}

// TestReleasedMigrationsAreContentPinned refuses any change that reuses a released
// version number under different content: already-stamped workspaces keep the
// old content under that number, so goose skips the new content forever.
//
// [LAW:single-enforcer] the one static gate on version-number reuse for v2+; v1 is
// enforced by baseline_frozen_test.go.
// [LAW:no-silent-failure] every divergence between the pins and the on-disk registry
// stops the build with a message naming the collision, so the drift can never reach a
// workspace unnoticed.
func TestReleasedMigrationsAreContentPinned(t *testing.T) {
	registry, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []migrationFile
	for _, m := range registry.Migrations() {
		if m.Version == Baseline {
			// [LAW:single-enforcer] v1's content is pinned by baseline_frozen_test.go.
			continue
		}
		data, err := FS.ReadFile(m.Name)
		if err != nil {
			t.Fatalf("read %q: %v", m.Name, err)
		}
		onDisk = append(onDisk, migrationFileOf(m.Version, m.Name, data))
	}
	for _, finding := range detectVersionReuse(pinnedVersionContent, onDisk) {
		t.Errorf("%s", finding.explain())
	}
}

// migrationFileOf reduces one migration file's bytes to the identity the pin
// compares: its Up section's sha256.
func migrationFileOf(version int64, name string, data []byte) migrationFile {
	sum := sha256.Sum256(upSection(data))
	return migrationFile{version: version, name: name, upSHA256: hex.EncodeToString(sum[:])}
}

// upSection returns the bytes goose runs as the Up: every line before the
// Down annotation, byte for byte, plus any `NO TRANSACTION` annotation after
// it, since goose applies that one to the whole file. Annotations are
// recognized as goose's sqlparser recognizes them, with no comment or quote
// tracking, so the split lands where goose's does.
// [LAW:one-source-of-truth] the producer is goose; this mirrors its parser.
func upSection(data []byte) []byte {
	var kept []string
	inDown := false
	for _, line := range strings.Split(string(data), "\n") {
		annotation, ok := gooseAnnotation(line)
		switch {
		case ok && strings.EqualFold(annotation, "Down"):
			inDown = true
		case !inDown, ok && strings.EqualFold(annotation, "NO TRANSACTION"):
			kept = append(kept, line)
		}
	}
	return []byte(strings.Join(kept, "\n"))
}

// gooseAnnotation returns the annotation a line carries as goose v3's
// sqlparser reads it: a line whose trimmed text starts with `--` and that
// contains `+goose`, with every `--` and the first `+goose` removed.
func gooseAnnotation(line string) (string, bool) {
	if !strings.HasPrefix(strings.TrimSpace(line), "--") || !strings.Contains(line, "+goose") {
		return "", false
	}
	cmd := strings.Replace(strings.ReplaceAll(line, "--", ""), "+goose", "", 1)
	return strings.TrimSpace(cmd), true
}

// TestUpSection pins what the Up-section hash sees: a Down edit is invisible
// to it, and any byte outside a Down section is not.
func TestUpSection(t *testing.T) {
	const released = "-- header\n-- +goose Up\nALTER TABLE t ADD c INT;\n\n-- +goose Down\nALTER TABLE t DROP c;\n"
	cases := []struct {
		name string
		body string
		same bool
	}{
		{"Down body rewritten", "-- header\n-- +goose Up\nALTER TABLE t ADD c INT;\n\n-- +goose Down\nALTER TABLE t DROP COLUMN c;\nSELECT 1;\n", true},
		// A missing Down is TestEveryMigrationHasDownSection's to refuse.
		{"Down section removed", "-- header\n-- +goose Up\nALTER TABLE t ADD c INT;\n", true},
		{"Up byte changed", "-- header\n-- +goose Up\nALTER TABLE t ADD d INT;\n\n-- +goose Down\nALTER TABLE t DROP c;\n", false},
		{"header comment changed", "-- header!\n-- +goose Up\nALTER TABLE t ADD c INT;\n\n-- +goose Down\nALTER TABLE t DROP c;\n", false},
		{"Down directive spelled without spaces", "-- header\n-- +goose Up\nALTER TABLE t ADD c INT;\n\n--+goose Down\nALTER TABLE t DROP c;\n", true},
		// goose reads NO TRANSACTION anywhere in the file and runs the Up by it.
		{"NO TRANSACTION added under Down", "-- header\n-- +goose Up\nALTER TABLE t ADD c INT;\n\n-- +goose Down\n-- +goose NO TRANSACTION\nALTER TABLE t DROP c;\n", false},
	}
	want := string(upSection([]byte(released)))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(upSection([]byte(tc.body))) == want; got != tc.same {
				t.Errorf("Up section unchanged = %v, want %v\nreleased Up: %q\nedited Up:   %q", got, tc.same, want, upSection([]byte(tc.body)))
			}
		})
	}
}

// TestReleasedDownIsFixableInPlace is the acceptance case on a real released
// migration: rewriting its Down passes the pin, and one changed byte of its Up
// fails it.
func TestReleasedDownIsFixableInPlace(t *testing.T) {
	const name = "00002_add_lane.sql"
	data, err := FS.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	pins := map[int64]pinnedMigration{2: pinnedVersionContent[2]}
	text := string(data)
	upAt := strings.Index(text, "-- +goose Up\n")
	downAt := strings.Index(text, "-- +goose Down\n")
	if upAt < 0 || downAt < upAt {
		t.Fatalf("%s lacks an Up directive followed by a Down directive", name)
	}

	downEdited := text[:downAt] + "-- +goose Down\nSELECT 1;\n"
	if findings := detectVersionReuse(pins, []migrationFile{migrationFileOf(2, name, []byte(downEdited))}); len(findings) != 0 {
		t.Errorf("rewriting the Down of %s failed the pin: %s", name, findings[0].explain())
	}

	upAt += len("-- +goose Up\n")
	upEdited := text[:upAt] + " " + text[upAt:]
	findings := detectVersionReuse(pins, []migrationFile{migrationFileOf(2, name, []byte(upEdited))})
	if len(findings) != 1 || findings[0].kind != kindContentChanged {
		t.Errorf("editing one byte of the Up of %s: got findings %+v, want one content change", name, findings)
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
	pin := func(file, sha string) pinnedMigration { return pinnedMigration{file: file, upSHA256: sha} }
	file := func(v int64, name, sha string) migrationFile {
		return migrationFile{version: v, name: name, upSHA256: sha}
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
	pin := pinnedMigration{file: "00007_x.sql", upSHA256: "cafef00d"}
	one := migrationFile{version: 7, name: "00007_x.sql", upSHA256: "deadbeef"}

	// Each fixture mirrors exactly what detectVersionReuse produces for that kind,
	// so explain() is exercised against the real onDisk/pinned shapes.
	cases := []struct {
		kind    reuseKind
		finding reuseFinding
	}{
		{kindContentChanged, reuseFinding{kind: kindContentChanged, version: 7, pinned: pin, onDisk: one}},
		{kindRenamed, reuseFinding{kind: kindRenamed, version: 7, pinned: pin, onDisk: one}},
		{kindUnpinned, reuseFinding{kind: kindUnpinned, version: 7, onDisk: one}},
		{kindDeleted, reuseFinding{kind: kindDeleted, version: 7, pinned: pin}},
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
