package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

const cmdFault = "fault"

// faultServices maps a friendly service name to the path prefix its requests
// carry. gRPC-first services map to their gRPC method prefix (which is what a
// client library sends); REST services map to their URL prefix. A trailing *
// is added when the rule is built, so the prefix covers the whole service.
var faultServices = map[string]string{
	"pubsub":        "/google.pubsub.v1.",
	"firestore":     "/google.firestore.v1.",
	"tasks":         "/google.cloud.tasks.v2.",
	"scheduler":     "/google.cloud.scheduler.v1.",
	"secretmanager": "/google.cloud.secretmanager.v1.",
	"secrets":       "/google.cloud.secretmanager.v1.",
	"gke":           "/google.container.v1.",
	"container":     "/google.container.v1.",
	"logging":       "/google.logging.v2.",
	"storage":       "/storage/v1/",
}

const faultUsage = `cloudrig fault - inject failures into a running emulator

usage:
  cloudrig fault <service> [--error N | --latency DUR | --timeout] [--failure-rate PCT]
  cloudrig fault list                 show the armed faults
  cloudrig fault clear                disarm every fault

services: pubsub, firestore, tasks, scheduler, secretmanager, gke, logging,
storage. Use --path to target any prefix directly.

flags:
  --error N          fail with HTTP status N (mapped to the matching gRPC code)
  --latency DUR      delay before responding, e.g. 2s (on the virtual clock)
  --timeout          fail as a timeout (504 / DeadlineExceeded)
  --failure-rate P   fail a deterministic fraction, e.g. 20% (1 in 5), not random
  --count N          fail only the first N matching requests, then let them through
  --message S        error message the fault returns
  --path PREFIX      match this path prefix instead of a named service
  --endpoint URL     emulator to talk to (default http://localhost:4599)

With no --error/--latency/--timeout a fault returns 503 (Unavailable).`

func runFaultCommand(args []string, env lookupEnv, stdout, stderr *os.File) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", faultUsage)
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, faultUsage)
		return flag.ErrHelp
	case "list":
		return faultList(args[1:], env, stdout, stderr)
	case "clear":
		return faultClear(args[1:], env, stdout, stderr)
	default:
		return faultAdd(args[0], args[1:], env, stdout, stderr)
	}
}

func faultAdd(service string, args []string, env lookupEnv, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("cloudrig fault "+service, flag.ContinueOnError)
	fs.SetOutput(stderr)
	errorCode := fs.Int("error", 0, "fail with this HTTP status")
	latency := fs.String("latency", "", "delay before responding, e.g. 2s")
	timeout := fs.Bool("timeout", false, "fail as a timeout (504 / DeadlineExceeded)")
	rate := fs.String("failure-rate", "", "fail a deterministic fraction, e.g. 20%")
	count := fs.Int("count", 0, "fail only the first N matching requests")
	message := fs.String("message", "", "error message the fault returns")
	path := fs.String("path", "", "match this path prefix instead of a named service")
	endpoint := fs.String("endpoint", "", "emulator to talk to (env CLOUDRIG_ENDPOINT)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	prefix := *path
	if prefix == "" {
		p, ok := faultServices[service]
		if !ok {
			return fmt.Errorf("unknown service %q; known: %s (or use --path)", service, knownServices())
		}
		prefix = p
	}

	rule := faultRule{Path: prefix + "*", Message: *message, Count: *count}
	switch {
	case *timeout:
		rule.Status = 504
	case *errorCode != 0:
		rule.Status = *errorCode
	}
	if *latency != "" {
		rule.Latency = *latency
	}
	if *rate != "" {
		r, err := parseRate(*rate)
		if err != nil {
			return err
		}
		rule.Rate = r
	}

	c := client{endpoint: resolveEndpoint(*endpoint, env)}
	if err := c.faultAdd(context.Background(), rule); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "armed: %s\n", describeFault(service, prefix, rule))
	return nil
}

func faultList(args []string, env lookupEnv, stdout, stderr *os.File) error {
	c, err := faultClient(args, env, stderr)
	if err != nil {
		return err
	}
	rules, err := c.faultList(context.Background())
	if err != nil {
		return err
	}
	if len(rules) == 0 {
		fmt.Fprintln(stdout, "no faults armed")
		return nil
	}
	for _, r := range rules {
		fmt.Fprintf(stdout, "%s\n", describeFault(serviceFor(r.Path), strings.TrimSuffix(r.Path, "*"), r))
	}
	return nil
}

func faultClear(args []string, env lookupEnv, stdout, stderr *os.File) error {
	c, err := faultClient(args, env, stderr)
	if err != nil {
		return err
	}
	if err := c.faultClear(context.Background()); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "cleared all faults")
	return nil
}

// faultClient parses the shared --endpoint flag for the argument-less
// subcommands.
func faultClient(args []string, env lookupEnv, stderr *os.File) (client, error) {
	fs := flag.NewFlagSet("cloudrig fault", flag.ContinueOnError)
	fs.SetOutput(stderr)
	endpoint := fs.String("endpoint", "", "emulator to talk to (env CLOUDRIG_ENDPOINT)")
	if err := fs.Parse(args); err != nil {
		return client{}, err
	}
	return client{endpoint: resolveEndpoint(*endpoint, env)}, nil
}

// parseRate accepts "20%", "20" or "0.2", all meaning one in five.
func parseRate(s string) (float64, error) {
	n, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
	if err != nil {
		return 0, fmt.Errorf("failure-rate %q: not a number", s)
	}
	if n > 1 {
		n /= 100
	}
	if n < 0 || n > 1 {
		return 0, fmt.Errorf("failure-rate %q: out of range", s)
	}
	return n, nil
}

func describeFault(service, prefix string, r faultRule) string {
	var what []string
	switch {
	case r.Status == 504:
		what = append(what, "timeout")
	case r.Status != 0:
		what = append(what, fmt.Sprintf("error %d", r.Status))
	}
	if r.Latency != "" {
		what = append(what, "latency "+r.Latency)
	}
	if len(what) == 0 {
		what = append(what, "error 503")
	}
	if r.Rate > 0 {
		what = append(what, fmt.Sprintf("%.0f%% of requests", r.Rate*100))
	}
	if r.Count > 0 {
		what = append(what, fmt.Sprintf("first %d", r.Count))
	}
	name := service
	if name == "" {
		name = prefix
	}
	return fmt.Sprintf("%s → %s  (%s)", name, strings.Join(what, ", "), prefix)
}

// serviceFor is the reverse of faultServices, for display in the list. It
// picks the first alias by name so output is stable when a prefix has several.
func serviceFor(path string) string {
	prefix := strings.TrimSuffix(path, "*")
	var match []string
	for name, p := range faultServices {
		if p == prefix {
			match = append(match, name)
		}
	}
	if len(match) == 0 {
		return ""
	}
	sort.Strings(match)
	return match[0]
}

func knownServices() string {
	names := make([]string, 0, len(faultServices))
	seen := map[string]bool{}
	for name, p := range faultServices {
		if seen[p] {
			continue
		}
		seen[p] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
