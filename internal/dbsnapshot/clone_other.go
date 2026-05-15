//go:build !darwin && !linux

package dbsnapshot

// cloneTree on platforms without a CoW implementation falls through to plain
// recursive copy.
//
// [LAW:dataflow-not-control-flow] Platform variability lives in which file Go
// links in (a value), not in a runtime branch inside one function.
func cloneTree(src, dst string) error {
	return walkAndCopy(src, dst, plainFileCopy)
}
