package functions

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Runtime names the language stack a function runs on, using the same
// identifiers as gcloud functions deploy --runtime.
type Runtime string

const (
	RuntimeGo     Runtime = "go"
	RuntimeNode20 Runtime = "nodejs20"
	RuntimeNode22 Runtime = "nodejs22"
)

// runtimes maps every accepted identifier onto its launcher. gcloud spells Go
// runtimes with a version (go121, go123); they all use the same toolchain here,
// whichever one is installed.
var runtimes = map[Runtime]launcher{
	RuntimeGo:     goLauncher{},
	"go121":       goLauncher{},
	"go122":       goLauncher{},
	"go123":       goLauncher{},
	"go124":       goLauncher{},
	"go125":       goLauncher{},
	RuntimeNode20: nodeLauncher{},
	RuntimeNode22: nodeLauncher{},
	"nodejs18":    nodeLauncher{},
}

// launcher prepares a function for execution. Runtimes differ in how the child
// reports readiness, so each one supplies its own matcher rather than the
// caller assuming a protocol.
type launcher interface {
	prepare(ctx context.Context, f Function) (spec, error)
}

// spec is a prepared, not yet started, function process.
type spec struct {
	cmd     *exec.Cmd
	cleanup func()

	// ready reports the serving address once a stdout line announces it. A Go
	// child picks its own port and prints it; a Node child is told which port
	// to use and only announces that it is up.
	ready func(line string) (addr string, ok bool)
}

// KnownRuntimes lists the accepted --runtime values, sorted.
func KnownRuntimes() []string {
	out := make([]string, 0, len(runtimes))
	for r := range runtimes {
		out = append(out, string(r))
	}
	sort.Strings(out)
	return out
}

// DetectRuntime infers the runtime from what is in the source directory, so
// --runtime is optional for the obvious cases.
func DetectRuntime(source string) (Runtime, error) {
	switch {
	case exists(filepath.Join(source, "go.mod")), hasSuffixFile(source, ".go"):
		return RuntimeGo, nil
	case exists(filepath.Join(source, "package.json")):
		return RuntimeNode20, nil
	}
	return "", fmt.Errorf("cannot tell the runtime of %s; pass --runtime (%s)",
		source, strings.Join(KnownRuntimes(), ", "))
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func hasSuffixFile(dir, suffix string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			return true
		}
	}
	return false
}

// reservePort asks the kernel for a free port and releases it.
//
// There is a race between releasing and the child binding, but runtimes whose
// framework will not report the port it actually bound leave no alternative.
func reservePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
