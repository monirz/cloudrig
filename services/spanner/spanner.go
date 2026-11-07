// Package spanner runs Google's Cloud Spanner emulator beside cloudrig.
//
// Spanner is not emulated here: its SQL, schema changes and transactions are
// far beyond what a stand-in should guess at, and Google ships an emulator
// for it. What this package adds is the running of it, so one
// `cloudrig start --with-spanner` brings up everything a pipeline needs and
// one Ctrl-C takes it down again.
package spanner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/core/tmp"
)

// DefaultImage is the emulator release cloudrig starts. Pinned, so an
// upstream release never changes a run without a cloudrig change.
const DefaultImage = "gcr.io/cloud-spanner-emulator/emulator:1.5.59"

// The emulator's own ports, inside the container.
const (
	containerGRPC = 9010
	containerREST = 9020
)

// Options say where the emulator listens on this machine.
type Options struct {
	Image    string // default DefaultImage
	GRPCPort int    // host port for gRPC, which the client libraries use; 0 picks one
	RESTPort int    // host port for REST, which gcloud uses; 0 picks one
}

// Emulator is a running Spanner emulator container.
type Emulator struct {
	cli *client.Client
	id  string

	// GRPCAddr is host:port for SPANNER_EMULATOR_HOST.
	GRPCAddr string
	// RESTAddr is host:port for gcloud's endpoint override.
	RESTAddr string
}

// Start pulls the image if needed, starts the container and waits until it
// accepts connections.
func Start(ctx context.Context, o Options) (*Emulator, error) {
	if o.Image == "" {
		o.Image = DefaultImage
	}
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("spanner: connecting to Docker: %w", err)
	}
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("spanner: --with-spanner runs Google's emulator in Docker, which is not reachable: %w", err)
	}
	if err := ensureImage(ctx, cli, o.Image); err != nil {
		_ = cli.Close()
		return nil, err
	}
	// A cloudrig killed outright never ran its deferred Stop, so its
	// emulator may still hold the ports this one needs.
	removeOrphans(ctx, cli)
	host, _ := os.Hostname()

	grpcPort, _ := network.PortFrom(containerGRPC, network.TCP)
	restPort, _ := network.PortFrom(containerREST, network.TCP)
	loopback := netip.MustParseAddr("127.0.0.1")
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:        o.Image,
			ExposedPorts: network.PortSet{grpcPort: struct{}{}, restPort: struct{}{}},
			Labels: map[string]string{
				roleLabel: roleValue,
				pidLabel:  strconv.Itoa(os.Getpid()),
				hostLabel: host,
			},
		},
		HostConfig: &container.HostConfig{
			PortBindings: network.PortMap{
				grpcPort: {{HostIP: loopback, HostPort: strconv.Itoa(o.GRPCPort)}},
				restPort: {{HostIP: loopback, HostPort: strconv.Itoa(o.RESTPort)}},
			},
		},
	})
	if err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("spanner: creating the emulator container: %w", err)
	}
	e := &Emulator{cli: cli, id: created.ID}

	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		_ = e.Stop(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("spanner: starting the emulator (is port %d or %d taken?): %w",
			o.GRPCPort, o.RESTPort, err)
	}

	inspected, err := cli.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
	if err != nil {
		_ = e.Stop(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("spanner: inspecting the emulator: %w", err)
	}
	published := func(p network.Port) string {
		b := inspected.Container.NetworkSettings.Ports[p]
		if len(b) == 0 {
			return ""
		}
		return net.JoinHostPort("localhost", b[0].HostPort)
	}
	e.GRPCAddr, e.RESTAddr = published(grpcPort), published(restPort)
	if e.GRPCAddr == "" || e.RESTAddr == "" {
		_ = e.Stop(context.WithoutCancel(ctx))
		return nil, errors.New("spanner: the emulator container published no ports")
	}

	if err := awaitREST(ctx, e.RESTAddr); err != nil {
		_ = e.Stop(context.WithoutCancel(ctx))
		return nil, err
	}
	return e, nil
}

// Stop removes the container, and with it everything the emulator held: it
// keeps no state on disk.
func (e *Emulator) Stop(ctx context.Context) error {
	_, err := e.cli.ContainerRemove(ctx, e.id, client.ContainerRemoveOptions{Force: true})
	_ = e.cli.Close()
	if err != nil {
		return fmt.Errorf("spanner: removing the emulator container: %w", err)
	}
	return nil
}

// Labels on the emulator container: what it is, and the cloudrig that owns it.
const (
	roleLabel = "cloudrig.role"
	roleValue = "spanner-emulator"
	pidLabel  = "cloudrig.pid"
	hostLabel = "cloudrig.host"
)

// removeOrphans removes emulator containers whose cloudrig is gone: started
// on this machine by a process no longer running. One owned by a live
// cloudrig, by another machine sharing the daemon, or carrying no owner is
// left alone. Failures are ignored; at worst the ports stay taken and Start
// says so.
func removeOrphans(ctx context.Context, cli *client.Client) {
	host, err := os.Hostname()
	if err != nil {
		return
	}
	list, err := cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", roleLabel+"="+roleValue),
	})
	if err != nil {
		return
	}
	for _, c := range list.Items {
		pid, err := strconv.Atoi(c.Labels[pidLabel])
		if err != nil || c.Labels[hostLabel] != host || tmp.Alive(pid) {
			continue
		}
		_, _ = cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true})
	}
}

func ensureImage(ctx context.Context, cli *client.Client, ref string) error {
	if _, err := cli.ImageInspect(ctx, ref); err == nil {
		return nil
	}
	body, err := cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("spanner: pulling %s: %w", ref, err)
	}
	defer body.Close()
	// The pull is only complete once its progress stream ends.
	if _, err := io.Copy(io.Discard, body); err != nil {
		return fmt.Errorf("spanner: pulling %s: %w", ref, err)
	}
	return nil
}

// awaitREST waits until the emulator answers HTTP on its REST port. Docker's
// port proxy accepts a connection before the emulator listens, so a plain
// dial proves nothing; a response does.
func awaitREST(ctx context.Context, addr string) error {
	const startup = time.Minute
	deadline := after(startup)
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.SetDeadline(clock.Real().Now().Add(2 * time.Second))
			_, _ = io.WriteString(conn, "GET /v1/projects/p/instanceConfigs HTTP/1.0\r\n\r\n")
			buf := make([]byte, 5)
			n, _ := io.ReadFull(conn, buf)
			_ = conn.Close()
			if n == len(buf) && string(buf) == "HTTP/" {
				return nil
			}
		}
		select {
		case <-deadline:
			return fmt.Errorf("spanner: the emulator did not answer on %s within %s", addr, startup)
		case <-ctx.Done():
			return ctx.Err()
		case <-after(200 * time.Millisecond):
			// A refused connection comes back at once; without a pause the
			// loop would spin a core while the emulator starts.
		}
	}
}

// after closes its channel once d has passed on the real clock: the emulator
// starts in real time, whatever cloudrig's own clock says.
func after(d time.Duration) <-chan struct{} {
	done := make(chan struct{})
	clock.Real().AfterFunc(d, func() { close(done) })
	return done
}
