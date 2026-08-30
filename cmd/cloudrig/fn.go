package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/monirz/cloudrig"
	"github.com/monirz/cloudrig/functions"
)

// fnConfig is everything `cloudrig fn run` takes.
type fnConfig struct {
	source     string
	name       string
	runtime    string
	entryPoint string
	port       int
}

// parseFnRun parses `fn run <dir> [flags]`. Name defaults to the directory's
// base and entry point is detected when the package has exactly one candidate.
func parseFnRun(args []string, env lookupEnv, out io.Writer) (fnConfig, error) {
	if len(args) == 0 || args[0] != "run" {
		return fnConfig{}, fmt.Errorf("usage: cloudrig fn run <dir> [--name N] [--entry-point F] [--port N]")
	}

	fs := flag.NewFlagSet("cloudrig fn run", flag.ContinueOnError)
	fs.SetOutput(out)

	port := defaultPort
	if v, ok := env("CLOUDRIG_PORT"); ok {
		n, err := atoiEnv("CLOUDRIG_PORT", v)
		if err != nil {
			return fnConfig{}, err
		}
		port = n
	}

	var c fnConfig
	fs.StringVar(&c.source, "source", "", "source directory (may also be given positionally)")
	fs.StringVar(&c.name, "name", "", "URL segment to serve the function under (default: the directory name)")
	fs.StringVar(&c.runtime, "runtime", "", "runtime: "+strings.Join(functions.KnownRuntimes(), ", ")+" (default: detected)")
	fs.StringVar(&c.entryPoint, "entry-point", "", "handler to serve (Go: detected when there is only one)")
	fs.IntVar(&port, "port", port, "port to listen on (env CLOUDRIG_PORT)")

	// flag stops parsing at the first non-flag argument, so a leading <dir>
	// would swallow every flag after it. Pull it off first, and still accept a
	// trailing one.
	rest := args[1:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		c.source, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return fnConfig{}, err
	}
	switch trailing := fs.Args(); {
	case c.source == "" && len(trailing) == 1:
		c.source = trailing[0]
	case c.source == "" && len(trailing) == 0:
		return fnConfig{}, fmt.Errorf("cloudrig fn run needs a source directory")
	case len(trailing) > 0:
		return fnConfig{}, fmt.Errorf("unexpected argument %q", trailing[0])
	}
	c.port = port

	if c.name == "" {
		abs, err := filepath.Abs(c.source)
		if err != nil {
			return fnConfig{}, err
		}
		c.name = filepath.Base(abs)
	}
	// Runtime and entry point are resolved by the functions package, so the
	// CLI and the library report the same errors.
	return c, nil
}

// fnUsage lists the fn subcommands.
const fnUsage = `usage:
  cloudrig fn run <dir> [--name N] [--runtime R] [--entry-point F] [--port N]
  cloudrig fn deploy <name> --source DIR [--runtime R] [--entry-point F]
  cloudrig fn list
  cloudrig fn describe <name>
  cloudrig fn delete <name>`

// runFnCommand dispatches the fn subcommands. run is self-contained; the rest
// are clients of an emulator that is already running.
func runFnCommand(args []string, env lookupEnv, stdout, stderr *os.File) error {
	if len(args) == 0 {
		return errors.New(fnUsage)
	}
	switch args[0] {
	case "run":
		return runFn(args, env, stdout, stderr)
	case "deploy":
		return fnDeploy(args[1:], env, stdout, stderr)
	case "list":
		return fnList(args[1:], env, stdout, stderr)
	case "describe":
		return fnDescribe(args[1:], env, stdout, stderr)
	case "delete":
		return fnDelete(args[1:], env, stdout, stderr)
	default:
		return fmt.Errorf("unknown fn subcommand %q\n%s", args[0], fnUsage)
	}
}

// deployFlags is the shape shared by every client subcommand.
type deployFlags struct {
	name       string
	source     string
	runtime    string
	entryPoint string
	project    string
	location   string
	endpoint   string
}

// fnDeploy sends a function to a running emulator.
func fnDeploy(args []string, env lookupEnv, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("cloudrig fn deploy", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var f deployFlags
	fs.StringVar(&f.source, "source", "", "source directory")
	fs.StringVar(&f.runtime, "runtime", "", "runtime: "+strings.Join(functions.KnownRuntimes(), ", ")+" (default: detected)")
	fs.StringVar(&f.entryPoint, "entry-point", "", "handler to serve")
	fs.StringVar(&f.project, "project", "", "GCP project (default "+functions.DefaultProject+")")
	fs.StringVar(&f.location, "region", "", "GCP location (default "+functions.DefaultLocation+")")
	fs.StringVar(&f.endpoint, "endpoint", "", "emulator endpoint (env CLOUDRIG_ENDPOINT)")

	name, rest := splitPositional(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if name == "" {
		if name, _ = splitPositional(fs.Args()); name == "" {
			return errors.New("cloudrig fn deploy needs a function name")
		}
	}
	if f.source == "" {
		return errors.New("cloudrig fn deploy needs --source")
	}

	// The emulator resolves the source itself, but it may have a different
	// working directory, so send an absolute path.
	source, err := filepath.Abs(f.source)
	if err != nil {
		return err
	}

	c := client{endpoint: resolveEndpoint(f.endpoint, env)}
	desc, err := c.deploy(context.Background(), functions.DeployRequest{
		Project:    f.project,
		Location:   f.location,
		Name:       name,
		Source:     source,
		Runtime:    functions.Runtime(f.runtime),
		EntryPoint: f.entryPoint,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "deployed %s (%s, %s)\n", desc.ResourceName(), desc.Runtime, desc.EntryPoint)
	fmt.Fprintf(stdout, "url: %s/%s-%s/%s\n", c.endpoint, desc.Location, desc.Project, desc.Name)
	return nil
}

func fnList(args []string, env lookupEnv, stdout, stderr *os.File) error {
	c, sc, err := clientOnly("cloudrig fn list", args, env, stderr)
	if err != nil {
		return err
	}
	descs, err := c.list(context.Background(), sc)
	if err != nil {
		return err
	}
	if len(descs) == 0 {
		fmt.Fprintln(stdout, "no functions deployed")
		return nil
	}

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tREGION\tPROJECT\tRUNTIME\tENTRY POINT\tSTATE")
	for _, d := range descs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			d.Name, d.Location, d.Project, d.Runtime, d.EntryPoint, d.State)
	}
	return w.Flush()
}

func fnDescribe(args []string, env lookupEnv, stdout, stderr *os.File) error {
	name, rest := splitPositional(args)
	if name == "" {
		return errors.New("cloudrig fn describe needs a function name")
	}
	c, sc, err := clientOnly("cloudrig fn describe", rest, env, stderr)
	if err != nil {
		return err
	}
	desc, err := c.describe(context.Background(), sc, name)
	if err != nil {
		return err
	}

	out, err := json.MarshalIndent(desc, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, string(out))
	return nil
}

func fnDelete(args []string, env lookupEnv, stdout, stderr *os.File) error {
	name, rest := splitPositional(args)
	if name == "" {
		return errors.New("cloudrig fn delete needs a function name")
	}
	c, sc, err := clientOnly("cloudrig fn delete", rest, env, stderr)
	if err != nil {
		return err
	}
	if err := c.delete(context.Background(), sc, name); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "deleted %s\n", name)
	return nil
}

// clientOnly parses a subcommand that only scopes and addresses a request.
func clientOnly(name string, args []string, env lookupEnv, stderr *os.File) (client, scope, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	endpoint := fs.String("endpoint", "", "emulator endpoint (env CLOUDRIG_ENDPOINT)")
	project := fs.String("project", "", "GCP project (default "+functions.DefaultProject+")")
	region := fs.String("region", "", "GCP location (default "+functions.DefaultLocation+")")
	if err := fs.Parse(args); err != nil {
		return client{}, scope{}, err
	}
	if rest := fs.Args(); len(rest) > 0 {
		return client{}, scope{}, fmt.Errorf("unexpected argument %q", rest[0])
	}
	return client{endpoint: resolveEndpoint(*endpoint, env)}, scope{*project, *region}, nil
}

// splitPositional pulls a leading non-flag argument off, because flag stops
// parsing at the first one.
func splitPositional(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// runFn builds the function, serves it through the emulator, and waits for a
// signal.
func runFn(args []string, env lookupEnv, stdout, stderr *os.File) error {
	cfg, err := parseFnRun(args, env, stderr)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(stdout, "starting %s from %s\n", cfg.name, cfg.source)
	emu, err := cloudrig.Start(ctx, cloudrig.Options{
		Addr:    fmt.Sprintf(":%d", cfg.port),
		Version: version,
		Runner:  "subprocess",
		Functions: []functions.Function{{
			Name:       cfg.name,
			Source:     cfg.source,
			Runtime:    functions.Runtime(cfg.runtime),
			EntryPoint: cfg.entryPoint,
		}},
		FunctionLog: stderr,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "cloudrig %s listening on %s\n", version, emu.BaseURL())
	fmt.Fprintf(stdout, "function: %s\n", emu.FunctionURL(cfg.name))

	<-ctx.Done()
	stop()
	fmt.Fprintln(stdout, "cloudrig: shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	return emu.Shutdown(shutdownCtx)
}
