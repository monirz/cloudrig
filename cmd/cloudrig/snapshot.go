package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

const (
	cmdSnapshot = "snapshot"
	cmdRestore  = "restore"
)

const snapshotUsage = `cloudrig snapshot - save a running emulator's state to a file

usage:
  cloudrig snapshot <file>   save state to file ("-" for stdout)
  cloudrig restore  <file>   load state from file ("-" for stdin)

A snapshot captures store state: buckets and objects, Pub/Sub, Firestore, Tasks
and the rest. Deployed functions, armed faults and the clock stay behind, as
with an in-process fork. Restoring replaces the target's state with the file's.

  --endpoint URL   emulator to talk to (default http://localhost:4599,
                   env CLOUDRIG_ENDPOINT)`

func runSnapshotCommand(args []string, env lookupEnv, stdout, stderr *os.File) error {
	c, file, err := snapshotFlags(cmdSnapshot, args, env, stderr)
	if err != nil {
		return err
	}

	out := stdout
	if file != "-" {
		f, err := os.Create(file)
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	}
	if err := c.snapshotSave(context.Background(), out); err != nil {
		return err
	}
	if file != "-" {
		fmt.Fprintf(stdout, "saved snapshot to %s\n", file)
	}
	return nil
}

func runRestoreCommand(args []string, env lookupEnv, stdout, stderr *os.File) error {
	c, file, err := snapshotFlags(cmdRestore, args, env, stderr)
	if err != nil {
		return err
	}

	in := os.Stdin
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}
	if err := c.snapshotRestore(context.Background(), in); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "restored")
	return nil
}

// snapshotFlags parses --endpoint and returns the client and the file argument.
func snapshotFlags(sub string, args []string, env lookupEnv, stderr *os.File) (client, string, error) {
	fs := flag.NewFlagSet("cloudrig "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	endpoint := fs.String("endpoint", "", "emulator to talk to (env CLOUDRIG_ENDPOINT)")
	if err := fs.Parse(args); err != nil {
		return client{}, "", err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return client{}, "", fmt.Errorf("cloudrig %s needs a file\n%s", sub, snapshotUsage)
	}
	if len(rest) > 1 {
		return client{}, "", fmt.Errorf("cloudrig %s takes one file; got %q", sub, rest[1])
	}
	return client{endpoint: resolveEndpoint(*endpoint, env)}, rest[0], nil
}
