package spanner_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/moby/moby/client"

	"github.com/monirz/cloudrig/services/spanner"
)

// freePort asks the kernel for a port nothing is using.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestStartAndStop runs the real emulator image, so it needs Docker and the
// image (pulled on first use). It is skipped without a daemon.
func TestStartAndStop(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a container")
	}
	ctx := context.Background()
	cli, err := client.New(client.FromEnv)
	if err == nil {
		_, err = cli.Ping(ctx, client.PingOptions{})
		_ = cli.Close()
	}
	if err != nil {
		t.Skipf("Docker is not reachable: %v", err)
	}

	emu, err := spanner.Start(ctx, spanner.Options{GRPCPort: freePort(t), RESTPort: freePort(t)})
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = emu.Stop(context.Background())
		}
	})

	// The REST port answers an instance-config listing, the call gcloud
	// makes first.
	resp, err := http.Get("http://" + emu.RESTAddr + "/v1/projects/p/instanceConfigs")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "emulator-config") {
		t.Errorf("instanceConfigs = %d %s", resp.StatusCode, body)
	}
	if conn, err := net.Dial("tcp", emu.GRPCAddr); err != nil {
		t.Errorf("the gRPC port is not open: %v", err)
	} else {
		conn.Close()
	}

	if err := emu.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	stopped = true
	if resp, err := http.Get("http://" + emu.RESTAddr + "/v1/projects/p/instanceConfigs"); err == nil {
		resp.Body.Close()
		t.Error("the emulator still answers after Stop")
	}
}
