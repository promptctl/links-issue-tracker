package cli

import "strings"

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
