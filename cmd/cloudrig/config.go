package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"

	"github.com/monirz/cloudrig/functions"
)

// config is everything cloudrig start takes.
type config struct {
	port   int
	runner string
}

const (
	cmdStart = "start"
	cmdFn    = "fn"
)

// runnerModes are the accepted --runner values; all resolve to "none" today.
var runnerModes = []string{"auto", "subprocess", "none"}

const (
	defaultPort   = 4599
	defaultRunner = "auto"
)

// lookupEnv is os.LookupEnv's shape, injected so precedence is testable
// without mutating the real environment.
type lookupEnv func(string) (string, bool)

// errNoCommand is returned when nothing was asked for. main prints usage.
var errNoCommand = errors.New("missing command")

// parseConfig dispatches the command, then resolves flags over environment
// over defaults. Each flag's default is seeded from its CLOUDRIG_ twin first,
// so an explicit flag wins with no "was it set?" bookkeeping.
func parseConfig(args []string, env lookupEnv, out io.Writer) (config, error) {
	if len(args) == 0 {
		usage(out)
		return config{}, errNoCommand
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(out)
		return config{}, flag.ErrHelp
	case cmdStart:
	default:
		return config{}, fmt.Errorf("unknown command %q; try: cloudrig %s or cloudrig %s run", args[0], cmdStart, cmdFn)
	}

	fs := flag.NewFlagSet("cloudrig "+cmdStart, flag.ContinueOnError)
	fs.SetOutput(out)

	port := defaultPort
	if v, ok := env("CLOUDRIG_PORT"); ok {
		n, err := atoiEnv("CLOUDRIG_PORT", v)
		if err != nil {
			return config{}, err
		}
		port = n
	}
	runner := defaultRunner
	if v, ok := env("CLOUDRIG_RUNNER"); ok {
		runner = v
	}

	fs.IntVar(&port, "port", port, "port to listen on (env CLOUDRIG_PORT)")
	fs.StringVar(&runner, "runner", runner,
		fmt.Sprintf("function runner: %v (env CLOUDRIG_RUNNER)", runnerModes))

	if err := fs.Parse(args[1:]); err != nil {
		return config{}, err
	}
	if rest := fs.Args(); len(rest) > 0 {
		return config{}, fmt.Errorf("unexpected argument %q", rest[0])
	}

	c := config{port: port, runner: runner}
	return c, c.validate()
}

func usage(out io.Writer) {
	fmt.Fprintf(out, `cloudrig - a local emulator for Google Cloud APIs

usage:
  cloudrig %s [--port N] [--runner %v]

  cloudrig %s deploy <name> --source DIR [--runtime R] [--entry-point F]
  cloudrig %s list
  cloudrig %s describe <name>
  cloudrig %s delete <name>
  cloudrig %s run <dir> [--name N] [--runtime R] [--entry-point F] [--port N]

start flags:
  --port N          port to listen on (default %d, env CLOUDRIG_PORT)
  --runner MODE     function runner: %v (default %q, env CLOUDRIG_RUNNER)

fn flags:
  --source DIR      source directory
  --runtime R       %v (default: detected from the source)
  --entry-point F   handler to serve (Go: detected when there is only one)
  --endpoint URL    emulator to talk to (default %s, env CLOUDRIG_ENDPOINT)

fn deploy, list, describe and delete talk to a running emulator.
fn run starts its own and needs no daemon.

Every flag has a CLOUDRIG_ environment twin; an explicit flag wins.
`, cmdStart, runnerModes,
		cmdFn, cmdFn, cmdFn, cmdFn, cmdFn,
		defaultPort, runnerModes, defaultRunner,
		functions.KnownRuntimes(), defaultEndpoint)
}

// atoiEnv keeps the "not a number" wording identical wherever it is reported.
func atoiEnv(key, val string) (int, error) {
	n, err := strconv.Atoi(val)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: not a number", key, val)
	}
	return n, nil
}

func (c config) validate() error {
	if c.port < 0 || c.port > 65535 {
		return fmt.Errorf("--port %d is out of range", c.port)
	}
	for _, m := range runnerModes {
		if c.runner == m {
			return nil
		}
	}
	return fmt.Errorf("--runner %q is not one of %v", c.runner, runnerModes)
}

func (c config) addr() string { return fmt.Sprintf(":%d", c.port) }
