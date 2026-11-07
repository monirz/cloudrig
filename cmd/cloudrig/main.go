// Command cloudrig runs the emulator as a server: flags, environment and
// signals only. Everything it exposes is reachable from the library too.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/monirz/cloudrig"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/core/tmp"
	"github.com/monirz/cloudrig/services/spanner"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags)" ./cmd/cloudrig
var version = "dev"

// shutdownGrace is how long in-flight requests get before the process exits.
const shutdownGrace = 10 * time.Second

func main() {
	err := run(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr)
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return
	case errors.Is(err, errNoCommand):
		os.Exit(2) // usage already printed
	default:
		fmt.Fprintf(os.Stderr, "cloudrig: %v\n", err)
		os.Exit(1)
	}
}

// run is main with its dependencies injected, so startup is testable.
func run(args []string, env lookupEnv, stdout, stderr *os.File) error {
	if len(args) > 0 && args[0] == cmdFn {
		return runFnCommand(args[1:], env, stdout, stderr)
	}
	if len(args) > 0 && args[0] == cmdClock {
		return runClockCommand(args[1:], env, stdout, stderr)
	}
	if len(args) > 0 && args[0] == cmdFault {
		return runFaultCommand(args[1:], env, stdout, stderr)
	}
	if len(args) > 0 && args[0] == cmdSnapshot {
		return runSnapshotCommand(args[1:], env, stdout, stderr)
	}
	if len(args) > 0 && args[0] == cmdRestore {
		return runRestoreCommand(args[1:], env, stdout, stderr)
	}

	cfg, err := parseConfig(args, env, stderr)
	if err != nil {
		return err
	}

	// The first signal shuts down gracefully; a second restores the default
	// handler, so an impatient operator can still kill it.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	opts := cloudrig.Options{
		Addr:     cfg.addr(),
		Version:  version,
		Runner:   cfg.runner,
		DataDir:  cfg.dataDir,
		EventLog: stdout,
	}
	if cfg.clock == "virtual" {
		// A fake clock that holds still until a client travels it forward. That
		// is what lets cloudrig clock advance/goto drive scheduled tasks, TTLs
		// and ack deadlines. --clock-start seeds it for a reproducible run;
		// otherwise it starts at the real now so timestamps look realistic.
		opts.Clock = clock.NewFakeNow()
		if cfg.clockStart != "" {
			start, err := cfg.startTime() // already validated
			if err != nil {
				return err
			}
			opts.Clock = clock.NewFake(start)
		}
	}
	emu, err := cloudrig.Start(ctx, opts)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "cloudrig %s listening on %s\n", version, emu.BaseURL())
	if cfg.dataDir != "" {
		fmt.Fprintf(stdout, "persisting storage under %s\n", cfg.dataDir)
	}
	fmt.Fprintf(stdout, "health: %s/_emu/health\n", emu.BaseURL())

	if cfg.withSpanner {
		fmt.Fprintf(stdout, "starting the Spanner emulator (%s)\n", spanner.DefaultImage)
		sp, err := spanner.Start(ctx, spanner.Options{GRPCPort: cfg.spannerPort, RESTPort: cfg.spannerPort + 10})
		if err != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			defer cancel()
			_ = emu.Shutdown(shutdownCtx)
			return err
		}
		// Removed on the way out, however the run ends.
		defer func() { _ = sp.Stop(context.Background()) }()
		fmt.Fprintf(stdout, "spanner: SPANNER_EMULATOR_HOST=%s (gcloud: http://%s/)\n", sp.GRPCAddr, sp.RESTAddr)
	}

	<-ctx.Done()
	stop()
	fmt.Fprintln(stdout, "cloudrig: shutting down")

	// A fresh context: the one above is already cancelled by the signal.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := emu.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	// The server owns its process, so it can take the whole root with it.
	// A test binary cannot: its emulators share one process.
	_ = tmp.RemoveRoot()
	return nil
}
