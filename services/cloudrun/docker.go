package cloudrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"

	"github.com/monirz/cloudrig/core/logring"
)

// ContainerPort is what a Cloud Run container is told to listen on. Cloud Run
// sets $PORT to 8080 unless the service says otherwise, and the container
// publishes that port to one the host picks.
const ContainerPort = 8080

// docker is the daemon connection, opened once and shared.
type dockerClient struct {
	once sync.Once
	cli  *client.Client
	err  error
}

var daemon dockerClient

// connect opens the daemon connection, or reports why it cannot.
func connect() (*client.Client, error) {
	daemon.once.Do(func() {
		daemon.cli, daemon.err = client.NewClientWithOpts(
			client.FromEnv,
			client.WithAPIVersionNegotiation(),
		)
	})
	if daemon.err != nil {
		return nil, fmt.Errorf("connecting to Docker: %w", daemon.err)
	}
	return daemon.cli, nil
}

// DockerAvailable reports whether a container can be run here. Deploying an
// image without a daemon is a clear error rather than a confusing one.
func DockerAvailable(ctx context.Context) bool {
	cli, err := connect()
	if err != nil {
		return false
	}
	_, err = cli.Ping(ctx)
	return err == nil
}

// startContainer runs a service's image and waits for it to listen.
func startContainer(ctx context.Context, svc Service, o Options) (*Instance, error) {
	cli, err := connect()
	if err != nil {
		return nil, err
	}
	if _, err := cli.Ping(ctx); err != nil {
		return nil, fmt.Errorf("service %s: Docker is not reachable: %w", svc.Name, err)
	}

	if err := ensureImage(ctx, cli, svc.Image); err != nil {
		return nil, err
	}

	// Applied to the container, so a service that would be killed for
	// exceeding its memory is killed here too. Accepting the flag and
	// dropping it is worse than refusing it: the deploy looks right and the
	// limit silently is not there.
	memory, err := memoryBytes(svc.Memory)
	if err != nil {
		return nil, fmt.Errorf("service %s: %w", svc.Name, err)
	}
	cpus, err := cpuNanos(svc.CPU)
	if err != nil {
		return nil, fmt.Errorf("service %s: %w", svc.Name, err)
	}

	port := nat.Port(strconv.Itoa(ContainerPort) + "/tcp")
	created, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:        svc.Image,
			Env:          containerEnv(svc, o),
			ExposedPorts: nat.PortSet{port: struct{}{}},
			Labels: map[string]string{
				"cloudrig.service":  svc.Name,
				"cloudrig.project":  svc.Project,
				"cloudrig.location": svc.Location,
				"cloudrig.revision": svc.Revision(),
			},
		},
		&container.HostConfig{
			// Published to a port the host picks, so parallel tests and
			// several services never collide.
			PortBindings: nat.PortMap{port: []nat.PortBinding{
				{HostIP: bindIP(cli.DaemonHost()), HostPort: "0"},
			}},
			AutoRemove: false,
			Resources:  container.Resources{Memory: memory, NanoCPUs: cpus},
		},
		nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("creating a container for %s: %w", svc.Name, err)
	}

	remove := func() {
		_ = cli.ContainerRemove(context.WithoutCancel(ctx), created.ID,
			container.RemoveOptions{Force: true})
	}
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		remove()
		return nil, fmt.Errorf("starting %s: %w", svc.Name, err)
	}

	logs := logring.New(logring.DefaultLines)
	go streamLogs(context.WithoutCancel(ctx), cli, created.ID, logs, o.Stderr)

	addr, err := publishedAddr(ctx, cli, created.ID, port)
	if err != nil {
		remove()
		return nil, fmt.Errorf("service %s: %w", svc.Name, err)
	}

	stopped := containerExit(context.WithoutCancel(ctx), cli, created.ID)
	if err := awaitListen(ctx, addr, stopped); err != nil {
		tail := logs.Tail(20)
		remove()
		if tail != "" {
			return nil, fmt.Errorf("%s: %w; the container's output was:\n%s", svc.Name, err, tail)
		}
		return nil, fmt.Errorf("%s: %w", svc.Name, err)
	}

	target, _ := url.Parse("http://" + addr)
	return &Instance{
		name:        svc.Name,
		project:     svc.Project,
		location:    svc.Location,
		revision:    svc.Revision(),
		addr:        addr,
		proxy:       httputil.NewSingleHostReverseProxy(target),
		logs:        logs,
		containerID: created.ID,
		cleanup:     remove,
	}, nil
}

// containerEnv is what Cloud Run guarantees a container, plus the deploy's own.
func containerEnv(svc Service, o Options) []string {
	env := []string{
		"PORT=" + strconv.Itoa(ContainerPort),
		"K_SERVICE=" + svc.Name,
		"K_REVISION=" + svc.Revision(),
		"K_CONFIGURATION=" + svc.Name,
	}
	env = append(env, svc.Env...)
	return append(env, o.Env...)
}

// ensureImage pulls the image unless the daemon already has it. A local build
// that was never pushed is the common case in a local emulator, so a missing
// image is only fetched, never demanded.
func ensureImage(ctx context.Context, cli *client.Client, ref string) error {
	if _, err := cli.ImageInspect(ctx, ref); err == nil {
		return nil
	}

	body, err := cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("image %s is not present and could not be pulled: %w", ref, err)
	}
	defer body.Close()

	// The pull is only complete once its progress stream ends.
	if _, err := io.Copy(io.Discard, body); err != nil {
		return fmt.Errorf("pulling %s: %w", ref, err)
	}
	return nil
}

// publishedAddr reads the host port the daemon assigned.
func publishedAddr(ctx context.Context, cli *client.Client, id string, port nat.Port) (string, error) {
	inspected, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		return "", fmt.Errorf("inspecting the container: %w", err)
	}

	bindings := inspected.NetworkSettings.Ports[port]
	if len(bindings) == 0 {
		return "", fmt.Errorf("the container published no host port for %s", port)
	}
	// Reached at the daemon's host, which is this machine only when the
	// daemon is.
	return hostPort(dialHost(cli.DaemonHost(), bindings[0].HostIP), bindings[0].HostPort), nil
}

// containerExit closes the returned channel when the container stops, so the
// readiness wait fails at once on a container that dies at startup.
func containerExit(ctx context.Context, cli *client.Client, id string) *child {
	c := &child{done: make(chan struct{})}

	go func() {
		defer close(c.done)
		wait, errs := cli.ContainerWait(ctx, id, container.WaitConditionNotRunning)
		select {
		case status := <-wait:
			c.err = fmt.Errorf("the container exited with status %d", status.StatusCode)
		case err := <-errs:
			if err != nil && !errors.Is(err, context.Canceled) {
				c.err = err
			}
		}
	}()
	return c
}

// streamLogs copies the container's output into the log ring, so `cloudrig run
// logs` and a failed startup both have something to show.
func streamLogs(ctx context.Context, cli *client.Client, id string, logs io.Writer, also io.Writer) {
	body, err := cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true, ShowStderr: true, Follow: true,
	})
	if err != nil {
		return
	}
	defer body.Close()

	out := logs
	if also != nil {
		out = io.MultiWriter(logs, also)
	}
	// Multiplexed frames: an eight-byte header per chunk, which would
	// otherwise show up as control characters in the log.
	demultiplex(body, out)
}

// demultiplex strips Docker's stream framing. A container started without a
// TTY has its stdout and stderr interleaved with an 8-byte header each.
func demultiplex(r io.Reader, w io.Writer) {
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			return
		}
		size := int64(header[4])<<24 | int64(header[5])<<16 | int64(header[6])<<8 | int64(header[7])
		if _, err := io.CopyN(w, r, size); err != nil {
			return
		}
	}
}
