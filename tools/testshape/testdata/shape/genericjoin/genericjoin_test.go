package genericjoin

import "testing"

func TestNames(t *testing.T) { _ = Names() }

func TestShown(t *testing.T) { _ = Shown() }

func TestShownSlice(t *testing.T) { _ = ShownSlice() }

func TestShownField(t *testing.T) { _ = ShownField() }

func TestVisited(t *testing.T) { Visited() }

func TestKeptCalled(t *testing.T) { KeptCalled() }

func TestStringers(t *testing.T) { _ = Stringers() }

func TestEncoded(t *testing.T) { _, _ = Encoded() }
