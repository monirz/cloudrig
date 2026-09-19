package cloudrun

import (
	"net"
	"testing"
)

// TestHeldOpen tells Docker's proxy, which accepts and then drops the
// connection while the container is still starting, from a real server.
func TestHeldOpen(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		hold bool
		want bool
	}{
		"a proxy with nothing behind it": {hold: false, want: false},
		"a listening server":             {hold: true, want: true},
	} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		release, closed := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(closed)
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if tc.hold {
				<-release
			}
			conn.Close()
		}()

		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if !tc.hold {
			<-closed // probe after the drop, as a client would find it
		}
		if got := heldOpen(conn); got != tc.want {
			t.Errorf("%s: heldOpen = %v, want %v", name, got, tc.want)
		}
		close(release)
		conn.Close()
		ln.Close()
	}
}
