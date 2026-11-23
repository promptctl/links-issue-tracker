package fstwocallsdeep

import "testing"

// Save reaches os.WriteFile through write: fs, two calls deep.
func TestSaveWritesTwoCallsDown(t *testing.T) {
	_ = Save("unused")
}
