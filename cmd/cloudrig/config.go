package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"

	"github.com/monirz/cloudrig/functions"
)

// config is everything cloudrig start takes.
type config struct {
	port       int
	runner     string
	dataDir    string
	clock      string
	clockStart string
}

const (
	cmdStart = "start"
	cmdFn    = "fn"
)

// runnerModes are the accepted --runner values; all resolve to "none" today.
var runnerModes = []string{"auto", "subprocess", "none"}

// clockModes are the accepted --clock values. "real" tracks wall time;
// "virtual" freezes time and moves it only when a client travels it forward
// with cloudrig clock advance/goto.
var clockModes = []string{"real", "virtual"}

const (
	defaultPort   = 4599
	defaultRunner = "auto"
	defaultClock  = "real"
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

	dataDir := ""
	if v, ok := env("CLOUDRIG_DATA_DIR"); ok {
		dataDir = v
	}
	fs.StringVar(&dataDir, "data-dir", dataDir,
		"persist Cloud Storage under this directory (env CLOUDRIG_DATA_DIR)")

	clk := defaultClock
	if v, ok := env("CLOUDRIG_CLOCK"); ok {
		clk = v
	}
	fs.StringVar(&clk, "clock", clk,
		fmt.Sprintf("clock: %v (env CLOUDRIG_CLOCK); virtual enables cloudrig clock", clockModes))

	clockStart := ""
	if v, ok := env("CLOUDRIG_CLOCK_START"); ok {
		clockStart = v
	}
	fs.StringVar(&clockStart, "clock-start", clockStart,
		"virtual clock's start time, RFC3339 (env CLOUDRIG_CLOCK_START); default: now")

	if err := fs.Parse(args[1:]); err != nil {
		return config{}, err
	}
	if rest := fs.Args(); len(rest) > 0 {
		return config{}, fmt.Errorf("unexpected argument %q", rest[0])
	}

	c := config{port: port, runner: runner, dataDir: dataDir, clock: clk, clockStart: clockStart}
	return c, c.validate()
}

func usage(out io.Writer) {
	fmt.Fprintf(out, `cloudrig - a local emulator for Google Cloud APIs

usage:
  cloudrig %s [--port N] [--runner %v]

  cloudrig %s deploy <name> --source DIR [--runtime R] [--entry-point F]
                            [--watch] [--trigger-bucket B] [--trigger-topic T]
  cloudrig %s list
  cloudrig %s describe <name>
  cloudrig %s delete <name>
  cloudrig %s run <dir> [--name N] [--runtime R] [--entry-point F] [--port N]

  cloudrig clock                     show the clock (needs --clock virtual)
  cloudrig clock advance <duration>  travel forward, e.g. 30m
  cloudrig clock goto <RFC3339>      travel to a time, e.g. 2026-09-12T15:00:00Z

start flags:
  --port N          port to listen on (default %d, env CLOUDRIG_PORT)
  --runner MODE     function runner: %v (default %q, env CLOUDRIG_RUNNER)
  --data-dir DIR    persist Cloud Storage here (default: in memory)
  --clock MODE      real or virtual (default real, env CLOUDRIG_CLOCK);
                    virtual freezes time so cloudrig clock can travel it
  --clock-start T   virtual clock's start, RFC3339 (env CLOUDRIG_CLOCK_START)

fn flags:
  --source DIR      source directory
  --runtime R       %v (default: detected from the source)
  --entry-point F   handler to serve (Go: detected when there is only one)
  --watch           redeploy when the source changes
  --trigger-bucket B  run on changes to a Cloud Storage bucket
  --trigger-topic T   run on messages published to a Pub/Sub topic
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
	if !slices.Contains(runnerModes, c.runner) {
		return fmt.Errorf("--runner %q is not one of %v", c.runner, runnerModes)
	}
	if !slices.Contains(clockModes, c.clock) {
		return fmt.Errorf("--clock %q is not one of %v", c.clock, clockModes)
	}
	if c.clockStart != "" {
		if c.clock != "virtual" {
			return errors.New("--clock-start only applies with --clock virtual")
		}
		if _, err := c.startTime(); err != nil {
			return err
		}
	}
	return nil
}

// startTime is the virtual clock's seed. The zero time with ok=false means
// "seed from the real now"; a set --clock-start is parsed as RFC3339.
func (c config) startTime() (time.Time, error) {
	t, err := time.Parse(time.RFC3339, c.clockStart)
	if err != nil {
		return time.Time{}, fmt.Errorf("--clock-start %q: want RFC3339, e.g. 2026-01-01T00:00:00Z", c.clockStart)
	}
	return t, nil
}

func (c config) addr() string { return fmt.Sprintf(":%d", c.port) }
