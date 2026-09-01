package cloudrun

import "testing"

// A daemon reached over TCP or SSH publishes container ports on its own
// machine, not this one. Getting this wrong times out the readiness wait on a
// container that is serving, and proxies requests to the wrong host.
func TestDaemonAddressing(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		endpoint string
		bound    string
		wantBind string
		wantDial string
	}{
		{"a unix socket is this machine", "unix:///var/run/docker.sock", "127.0.0.1", "127.0.0.1", "127.0.0.1"},
		{"a windows pipe is this machine", "npipe:////./pipe/docker_engine", "127.0.0.1", "127.0.0.1", "127.0.0.1"},
		{"no endpoint at all", "", "127.0.0.1", "127.0.0.1", "127.0.0.1"},
		{"tcp on loopback is still this machine", "tcp://127.0.0.1:2375", "127.0.0.1", "127.0.0.1", "127.0.0.1"},
		{"tcp to a VM", "tcp://192.168.64.2:2375", "0.0.0.0", "0.0.0.0", "192.168.64.2"},
		{"ssh to another host", "ssh://user@builder.internal", "0.0.0.0", "0.0.0.0", "builder.internal"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bindIP(c.endpoint); got != c.wantBind {
				t.Errorf("bindIP = %q, want %q", got, c.wantBind)
			}
			if got := dialHost(c.endpoint, c.bound); got != c.wantDial {
				t.Errorf("dialHost = %q, want %q", got, c.wantDial)
			}
		})
	}
}

// TestUnspecifiedBindingIsLoopback covers a local daemon reporting 0.0.0.0,
// which cannot be dialled as an address.
func TestUnspecifiedBindingIsLoopback(t *testing.T) {
	t.Parallel()

	for _, bound := range []string{"", "0.0.0.0", "::"} {
		if got := dialHost("unix:///var/run/docker.sock", bound); got != "127.0.0.1" {
			t.Errorf("dialHost(local, %q) = %q, want 127.0.0.1", bound, got)
		}
	}
}

func TestHostPortBracketsIPv6(t *testing.T) {
	t.Parallel()

	if got := hostPort("::1", "8080"); got != "[::1]:8080" {
		t.Errorf("hostPort = %q, want [::1]:8080", got)
	}
	if got := hostPort("127.0.0.1", "8080"); got != "127.0.0.1:8080" {
		t.Errorf("hostPort = %q", got)
	}
}
