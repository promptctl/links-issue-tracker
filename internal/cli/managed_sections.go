package cli

import "strings"

// migrateMarkers replaces legacy begin/end marker pairs with current ones.
// Idempotent: if no legacy markers are present, the content is returned unchanged.
// Migrates whenever EITHER legacy marker is present so partial-state files
// (one marker manually edited away) still converge — leaving a stray legacy
// marker would cause upsertManagedSection to append a second managed section.
func migrateMarkers(content, oldBegin, oldEnd, newBegin, newEnd string) string {
	if !strings.Contains(content, oldBegin) && !strings.Contains(content, oldEnd) {
		return content
	}
	content = strings.ReplaceAll(content, oldBegin, newBegin)
	content = strings.ReplaceAll(content, oldEnd, newEnd)
	return content
}

// upsertManagedSection replaces the managed section when markers exist,
// otherwise appends the section to the end of the document.
func upsertManagedSection(content string, section string, beginMarker string, endMarker string) (string, bool) {
	start := strings.Index(content, beginMarker)
	end := strings.Index(content, endMarker)
	if start != -1 && end != -1 && start < end {
		lineStart := strings.LastIndex(content[:start], "\n")
		if lineStart == -1 {
			lineStart = 0
		} else {
			lineStart++
		}
		endOfMarker := end + len(endMarker)
		if newline := strings.Index(content[endOfMarker:], "\n"); newline != -1 {
			endOfMarker += newline + 1
		} else {
			endOfMarker = len(content)
		}
		updated := content[:lineStart] + section + content[endOfMarker:]
		return updated, updated != content
	}

	updated := content
	if strings.TrimSpace(updated) == "" {
		updated = section
	} else {
		if !strings.HasSuffix(updated, "\n") {
			updated += "\n"
		}
		updated += "\n" + section
	}
	return updated, updated != content
}

// removeManagedSection removes the marker-managed section if present.
func removeManagedSection(content string, beginMarker string, endMarker string) (string, bool) {
	start := strings.Index(content, beginMarker)
	end := strings.Index(content, endMarker)
	if start == -1 || end == -1 || start > end {
		return content, false
	}

	lineStart := strings.LastIndex(content[:start], "\n")
	if lineStart == -1 {
		lineStart = 0
	} else {
		lineStart++
	}
	endOfMarker := end + len(endMarker)
	if newline := strings.Index(content[endOfMarker:], "\n"); newline != -1 {
		endOfMarker += newline + 1
	} else {
		endOfMarker = len(content)
	}
	updated := content[:lineStart] + content[endOfMarker:]
	updated = strings.TrimRight(updated, "\n")
	if updated != "" {
		updated += "\n"
	}
	return updated, true
}
