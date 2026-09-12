package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
)

const cmdClock = "clock"

const clockUsage = `cloudrig clock - control a manual-mode emulator's time

usage:
  cloudrig clock                     show the clock (mode, now, pending timers)
  cloudrig clock freeze              confirm time is frozen (virtual mode)
  cloudrig clock advance <duration>  travel forward, e.g. 30m, 2h, 90s
  cloudrig clock goto <RFC3339>      travel to a time, e.g. 2026-09-12T15:00:00Z

Time travel needs a virtual-clock server: cloudrig start --clock virtual.
Time moves only forward. Advancing fires everything due in the jump — scheduled
jobs, tasks, ack deadlines and TTLs — so a test drives them without waiting.

  --endpoint URL   emulator to talk to (default http://localhost:4599,
                   env CLOUDRIG_ENDPOINT)`

func runClockCommand(args []string, env lookupEnv, stdout, stderr *os.File) error {
	if len(args) == 0 {
		return clockShow(nil, env, stdout, stderr)
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, clockUsage)
		return flag.ErrHelp
	case "status", "show":
		return clockShow(args[1:], env, stdout, stderr)
	case "freeze":
		return clockFreeze(args[1:], env, stdout, stderr)
	case "advance":
		return clockAdvance(args[1:], env, stdout, stderr)
	case "goto":
		return clockGoto(args[1:], env, stdout, stderr)
	default:
		return fmt.Errorf("unknown clock subcommand %q\n%s", args[0], clockUsage)
	}
}

// clockFlags parses the shared --endpoint flag and returns the rest.
func clockFlags(sub string, args []string, env lookupEnv, stderr *os.File) (client, []string, error) {
	fs := flag.NewFlagSet("cloudrig clock "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	endpoint := fs.String("endpoint", "", "emulator to talk to (env CLOUDRIG_ENDPOINT)")
	if err := fs.Parse(args); err != nil {
		return client{}, nil, err
	}
	return client{endpoint: resolveEndpoint(*endpoint, env)}, fs.Args(), nil
}

func clockShow(args []string, env lookupEnv, out, errOut *os.File) error {
	c, _, err := clockFlags("status", args, env, errOut)
	if err != nil {
		return err
	}
	s, err := c.clockStatus(context.Background())
	if err != nil {
		return err
	}
	printClock(out, s)
	return nil
}

func clockFreeze(args []string, env lookupEnv, out, errOut *os.File) error {
	c, _, err := clockFlags("freeze", args, env, errOut)
	if err != nil {
		return err
	}
	s, err := c.clockStatus(context.Background())
	if err != nil {
		return err
	}
	if s.Mode != "manual" {
		return errors.New("the clock is real; start the emulator with --clock virtual to travel time")
	}
	fmt.Fprintf(out, "clock frozen at %s (%d timers pending)\n", s.Now, s.Pending)
	return nil
}

func clockAdvance(args []string, env lookupEnv, out, errOut *os.File) error {
	c, rest, err := clockFlags("advance", args, env, errOut)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("usage: cloudrig clock advance <duration>, e.g. 30m")
	}
	s, err := c.clockAdvance(context.Background(), rest[0])
	if err != nil {
		return err
	}
	printClock(out, s)
	return nil
}

func clockGoto(args []string, env lookupEnv, out, errOut *os.File) error {
	c, rest, err := clockFlags("goto", args, env, errOut)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("usage: cloudrig clock goto <RFC3339>, e.g. 2026-09-12T15:00:00Z")
	}
	s, err := c.clockGoto(context.Background(), rest[0])
	if err != nil {
		return err
	}
	printClock(out, s)
	return nil
}

func printClock(out *os.File, s clockState) {
	fmt.Fprintf(out, "clock: %s  now: %s  pending: %d\n", s.Mode, s.Now, s.Pending)
}
