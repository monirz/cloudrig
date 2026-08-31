package cloudrun

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/monirz/cloudrig/core/tmp"
)

// build prepares the command that serves a service.
//
// Cloud Run deploys a container; from source it builds one with buildpacks,
// which for Go means "compile the main package" and for Node "npm install,
// then npm start". Those are the two steps reproduced here, without the
// container: what runs is the same program.
func build(ctx context.Context, svc Service, source string) (*exec.Cmd, func(), error) {
	abs, err := filepath.Abs(source)
	if err != nil {
		return nil, nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, nil, fmt.Errorf("source %s: %w", source, err)
	}

	switch {
	case exists(filepath.Join(abs, "go.mod")):
		return buildGo(ctx, abs)
	case exists(filepath.Join(abs, "package.json")):
		return buildNode(ctx, abs)
	}
	return nil, nil, fmt.Errorf("source %s: no go.mod or package.json, so there is nothing to build", source)
}

// buildGo compiles the main package to a binary of its own, so the running
// service does not depend on the source directory staying put.
func buildGo(ctx context.Context, source string) (*exec.Cmd, func(), error) {
	out, err := tmp.Dir("run")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(out) }

	bin := filepath.Join(out, "service")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, ".")
	build.Dir = source
	// A source directory with its own go.mod is not part of any workspace the
	// emulator happens to be running inside.
	build.Env = append(os.Environ(), "GOWORK=off")

	if output, err := build.CombinedOutput(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("building %s: %w\n%s", source, err, output)
	}

	// Not CommandContext: ctx bounds the build, not the service. A service
	// killed when its deploy request ended would never serve a request.
	cmd := exec.Command(bin)
	cmd.Dir = source
	return cmd, cleanup, nil
}

// buildNode installs dependencies and runs the start script, which is what a
// Node buildpack does.
func buildNode(ctx context.Context, source string) (*exec.Cmd, func(), error) {
	if exists(filepath.Join(source, "package-lock.json")) || exists(filepath.Join(source, "node_modules")) {
		install := exec.CommandContext(ctx, "npm", "install", "--no-audit", "--no-fund")
		install.Dir = source
		if output, err := install.CombinedOutput(); err != nil {
			return nil, nil, fmt.Errorf("npm install in %s: %w\n%s", source, err, output)
		}
	}

	cmd := exec.Command("npm", "start", "--silent")
	cmd.Dir = source
	return cmd, func() {}, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
