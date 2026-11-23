package testmainvalue

import (
	"os"
	"testing"
)

var load func() ([]byte, error)

func TestMain(m *testing.M) {
	load = ReadConfig
	os.Exit(m.Run())
}

func TestLoad(t *testing.T) { _, _ = load() }
