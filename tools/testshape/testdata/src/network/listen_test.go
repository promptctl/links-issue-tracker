// Package network opens a listener.
package network

import (
	"net"
	"testing"
)

func TestAListenerTouchesTheNetwork(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
}
