package functions

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// nodeReady is what functions-framework logs from inside its server.listen
// callback, so it means the socket is already accepting. The framework skips
// these lines when NODE_ENV is "production", which is why the child's
// environment must not set that.
const nodeReady = "Serving function..."

// nodeLauncher runs a function through @google-cloud/functions-framework,
// which must be installed in the function's own node_modules.
type nodeLauncher struct{}

func (nodeLauncher) prepare(ctx context.Context, f Function) (spec, error) {
	// Absolute, because cmd.Dir is the source directory and a relative binary
	// path would resolve against it a second time.
	source, err := filepath.Abs(f.Source)
	if err != nil {
		return spec{}, err
	}
	bin := filepath.Join(source, "node_modules", ".bin", "functions-framework")
	if !exists(bin) {
		return spec{}, fmt.Errorf("%s: %s not found in %s; run: npm i @google-cloud/functions-framework",
			f.Name, filepath.Join("node_modules", ".bin", "functions-framework"), f.Source)
	}

	// The framework echoes back whatever --port it was given rather than the
	// port it bound, so --port=0 is useless for discovery: reserve one instead.
	port, err := reservePort()
	if err != nil {
		return spec{}, fmt.Errorf("%s: reserving a port: %w", f.Name, err)
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)

	// Deliberately not CommandContext: ctx bounds preparation, but the child
	// outlives the call that started it. Tying it to a deploy request's context
	// kills the function the instant that request completes.
	cmd := exec.Command(bin,
		"--target="+f.EntryPoint,
		"--port="+strconv.Itoa(port),
		"--signature-type=http",
	)
	cmd.Dir = source

	return spec{
		cmd:     cmd,
		cleanup: func() {},
		ready: func(line string) (string, bool) {
			return addr, strings.Contains(line, nodeReady)
		},
	}, nil
}
