package trun

import "testing"

func TestSubtest(t *testing.T) {
	t.Run("writes", func(t *testing.T) { _ = Write("out") })
}
