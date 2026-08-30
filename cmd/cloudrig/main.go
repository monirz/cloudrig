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

	cfg, err := parseConfig(args, env, stderr)
	if err != nil {
		return err
	}

	// The first signal shuts down gracefully; a second restores the default
	// handler, so an impatient operator can still kill it.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	emu, err := cloudrig.Start(ctx, cloudrig.Options{
		Addr:    cfg.addr(),
		Version: version,
		Runner:  cfg.runner,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "cloudrig %s listening on %s\n", version, emu.BaseURL())
	fmt.Fprintf(stdout, "health: %s/_emu/health\n", emu.BaseURL())

	<-ctx.Done()
	stop()
	fmt.Fprintln(stdout, "cloudrig: shutting down")

	// A fresh context: the one above is already cancelled by the signal.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := emu.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
