package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/issueid"
)

// Limits: each verdict as a record a machine files, written the moment the
// verdict is reached.
//
// A red nightly run is the design reaching a limit, and the answer is a ticket
// naming which limit broke at what scale and by how much. The report on stdout
// says all of that, but only once every size has finished and only if the run
// got that far; a run that times out, or whose second scale point fails, would
// take every verdict it had already reached down with it. So each verdict is
// also appended to --limits as it is logged, keyed by the limit it breaks, and
// the nightly lane files one ticket per key from that file.
// [LAW:no-silent-failure]

// limitRecord is one verdict as the lane files it: limit is the key one
// ticket collects every night's breach of this limit under, and a lit topic
// slug, because the reporter labels and topics the ticket by it; measured is
// the verdict's own sentence, the line the log printed.
type limitRecord struct {
	Limit    string `json:"limit"`
	Title    string `json:"title"`
	Measured string `json:"measured"`
}

// limitVerdict is anything the gate fails on. logLine is what progress prints when
// it is reached; limit is the same verdict as a record.
type limitVerdict interface {
	logLine() string
	limit() limitRecord
}

// limitKey names the limit what breaks at the scale point s, in the slug form
// lit takes for a label and a topic. [LAW:single-enforcer] the slug rules are
// issueid's, the ones `lit new --topic` enforces, not a second copy here.
func limitKey(s size, what string) string {
	return issueid.NormalizeSlug(s.name + " " + what)
}

// logVerdict writes v to progress and its record to records, together, so a
// verdict reached is in both or the run fails. The record's measured sentence
// is the verdict's String, the same line the report and the gate's error
// print; the log line may carry more, as a failed writer's stderr.
// [LAW:one-source-of-truth]
func logVerdict(progress, records io.Writer, v limitVerdict) error {
	fmt.Fprintln(progress, v.logLine())
	line, err := json.Marshal(v.limit())
	if err != nil {
		return fmt.Errorf("encoding limit record: %w", err)
	}
	if _, err := fmt.Fprintf(records, "%s\n", line); err != nil {
		return fmt.Errorf("writing --limits: %w", err)
	}
	return nil
}

func (b breach) logLine() string { return "OVER BUDGET: " + b.String() }

func (b breach) limit() limitRecord {
	return limitRecord{
		Limit:    limitKey(b.store, b.probe),
		Title:    fmt.Sprintf("At %s, lit %s is over its %s latency budget", b.store, b.probe, b.budget),
		Measured: b.String(),
	}
}

func (b inputBreach) logLine() string { return "OVER ALLOWANCE: " + b.String() }

func (b inputBreach) limit() limitRecord {
	return limitRecord{
		Limit:    limitKey(b.store, b.input),
		Title:    fmt.Sprintf("At %s, the store's %s pass the size ceiling's allowance", b.store, b.input),
		Measured: b.String(),
	}
}

// A storm's failed writers are one limit, however many failed: "writers 3, 41
// and 77" is one contention bug, so the lane collects every failed writer of a
// night under one key.
func (f writerFailure) logLine() string { return "WRITER FAILED: " + f.full() }

func (f writerFailure) limit() limitRecord {
	return limitRecord{
		Limit:    limitKey(f.store.point, "storm"),
		Title:    fmt.Sprintf("At %s, %d concurrent writers do not all complete", f.store.point, stormWriters),
		Measured: f.String(),
	}
}
