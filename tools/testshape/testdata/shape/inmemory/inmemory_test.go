package inmemory

import "testing"

func TestMemory(t *testing.T) {
	var s Store = Memory{"k": "v"}
	_ = s.Get("k")
}

func TestDisk(t *testing.T) {
	s, _ := OpenDisk("store")
	_ = s.Get("k")
}
