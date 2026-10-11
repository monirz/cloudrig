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

A snapshot captures store state (buckets and objects, Pub/Sub topics and
subscriptions, Firestore, Tasks and the rest), armed faults, and a virtual
clock's reading. Deployed functions stay behind, as with an in-process fork.
Restoring replaces the target's state with the file's.

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
	// Flags may follow the file too, as they may follow a fault's service.
	file := rest[0]
	if err := fs.Parse(rest[1:]); err != nil {
		return client{}, "", err
	}
	if rest := fs.Args(); len(rest) > 0 {
		return client{}, "", fmt.Errorf("cloudrig %s takes one file; got %q", sub, rest[0])
	}
	return client{endpoint: resolveEndpoint(*endpoint, env)}, file, nil
}
