package cli

// DeprecationWarning returns a deprecation message when invoked as "lit".
func DeprecationWarning(calledAs string) string {
	if calledAs != "lit" {
		return ""
	}
	return "warning: `lit` is deprecated and will be removed on 2026-09-01. Use `lnks` instead.\n"
}
