package cli

import (
	"fmt"
	"strings"
)

// templateShapeError signals a managed template whose content is neither plain
// section text nor one whole marker-delimited block — a shape no reconcile can
// converge. It is its own type because the act it calls for is its own: the
// user edits or deletes a template file, and the generic validation advice
// ("adjust the command") would send an agent looping on a command that can
// never succeed while the file on disk is unchanged. [LAW:types-are-the-program]
type templateShapeError struct {
	Message string
}

func (e templateShapeError) Error() string { return e.Message }

// markerPair is the ownership boundary of one managed region: the begin/end
// comment pair that says which bytes of a user-owned file are lit's to rewrite.
type markerPair struct {
	begin string
	end   string
}

// managedSection is template content proven to be exactly one marker-delimited
// block, carrying the pair it was proven against.
//
// [LAW:parse-dont-validate] markerPair.parse is the only way to mint one, so a
// call site cannot hand upsertManagedSection unproven template text — which is
// the shape this bug took: the same normalization landed for the /next skill
// (#460) as a step callers had to remember, and the agents-section and
// pre-push-hook templates never got it (links-templates-1bai).
type managedSection struct {
	pair markerPair
	text string
}

// parse turns resolved template content into a managed section. Exactly two
// shapes are legal: content carrying no markers at all — plain guidance text,
// the convention every other managed template follows — is wrapped in the pair,
// and content that is exactly one whole BEGIN/END block passes through
// canonicalized.
//
// Every other shape is rejected rather than written. A lone marker, a stray
// extra one, or text outside the pair leaves part of itself outside the span
// upsertManagedSection replaces, so the managed file grows by that part on
// every run — unbounded growth of a file the user did not ask to grow.
// [LAW:no-silent-failure]
func (p markerPair) parse(content string) (managedSection, error) {
	begins := strings.Count(content, p.begin)
	ends := strings.Count(content, p.end)
	if begins == 0 && ends == 0 {
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		return managedSection{pair: p, text: p.begin + "\n" + content + p.end + "\n"}, nil
	}
	// The pass-through re-emits the trimmed block, not the raw bytes: whitespace
	// outside the marker span sits outside the replaced region too, and would
	// compound a blank line per run.
	trimmed := strings.TrimSpace(content)
	if begins == 1 && ends == 1 && strings.HasPrefix(trimmed, p.begin) && strings.HasSuffix(trimmed, p.end) {
		return managedSection{pair: p, text: trimmed + "\n"}, nil
	}
	return managedSection{}, templateShapeError{Message: fmt.Sprintf(
		"template must contain either no %s / %s markers or be exactly one such block with nothing outside it; found %d begin and %d end marker(s)",
		p.begin, p.end, begins, ends)}
}

// migrateMarkers rewrites a legacy marker pair in content to the current one.
// Each marker migrates on its own, so a partial-state file (one marker manually
// edited away) still converges — leaving a stray legacy marker behind would
// make upsertManagedSection append a second managed section.
// [LAW:dataflow-not-control-flow] Both replacements always run; content carrying
// no legacy marker is unchanged because the replacements find nothing, not
// because a guard skipped them.
func migrateMarkers(content string, legacy, current markerPair) string {
	content = strings.ReplaceAll(content, legacy.begin, current.begin)
	content = strings.ReplaceAll(content, legacy.end, current.end)
	return content
}

// upsertManagedSection replaces the managed section when the section's markers
// are present in content, otherwise appends the section to the end.
func upsertManagedSection(content string, section managedSection) (string, bool) {
	start := strings.Index(content, section.pair.begin)
	end := strings.Index(content, section.pair.end)
	if start != -1 && end != -1 && start < end {
		lineStart := strings.LastIndex(content[:start], "\n")
		if lineStart == -1 {
			lineStart = 0
		} else {
			lineStart++
		}
		endOfMarker := end + len(section.pair.end)
		if newline := strings.Index(content[endOfMarker:], "\n"); newline != -1 {
			endOfMarker += newline + 1
		} else {
			endOfMarker = len(content)
		}
		updated := content[:lineStart] + section.text + content[endOfMarker:]
		return updated, updated != content
	}

	updated := content
	if strings.TrimSpace(updated) == "" {
		updated = section.text
	} else {
		if !strings.HasSuffix(updated, "\n") {
			updated += "\n"
		}
		updated += "\n" + section.text
	}
	return updated, updated != content
}
