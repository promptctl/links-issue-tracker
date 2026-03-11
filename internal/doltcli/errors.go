package doltcli

import "strings"

func IsManifestReadOnlyError(err error) bool {
	if err == nil {
		return false
	}
	// [LAW:single-enforcer] Dolt manifest read-only classification is centralized here for all call sites.
	normalized := strings.ToLower(err.Error())
	return strings.Contains(normalized, "cannot update manifest") && strings.Contains(normalized, "read only")
}
