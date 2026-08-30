package main

import (
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
)

// env builds a lookupEnv over a map, so parallel tests do not share state.
func env(pairs map[string]string) lookupEnv {
	return func(k string) (string, bool) {
		v, ok := pairs[k]
		return v, ok
	}
}

func TestParseConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		wantPort   int
		wantRunner string
		wantErr    string
	}{
		{
			name:       "defaults",
			args:       []string{"start"},
			wantPort:   4599,
			wantRunner: "auto",
		},
		{
			name:       "flags",
			args:       []string{"start", "--port", "5000", "--runner", "none"},
			wantPort:   5000,
			wantRunner: "none",
		},
		{
			// docker run -e CLOUDRIG_PORT=5000 must equal --port 5000.
			name:       "env mirrors every flag",
			args:       []string{"start"},
			env:        map[string]string{"CLOUDRIG_PORT": "5000", "CLOUDRIG_RUNNER": "subprocess"},
			wantPort:   5000,
			wantRunner: "subprocess",
		},
		{
			name:       "an explicit flag beats the environment",
			args:       []string{"start", "--port", "6000"},
			env:        map[string]string{"CLOUDRIG_PORT": "5000", "CLOUDRIG_RUNNER": "none"},
			wantPort:   6000,
			wantRunner: "none",
		},
		{
			name:       "single-dash flags work too",
			args:       []string{"start", "-port", "5000"},
			wantPort:   5000,
			wantRunner: "auto",
		},
		{
			name:       "port 0 asks the kernel for a free port",
			args:       []string{"start", "--port", "0"},
			wantPort:   0,
			wantRunner: "auto",
		},
		{
			name:    "a non-numeric CLOUDRIG_PORT is an error, not a silent default",
			args:    []string{"start"},
			env:     map[string]string{"CLOUDRIG_PORT": "wat"},
			wantErr: `CLOUDRIG_PORT="wat": not a number`,
		},
		{
			name:    "an out-of-range port is rejected",
			args:    []string{"start", "--port", "70000"},
			wantErr: "out of range",
		},
		{
			name:    "an unknown runner is rejected",
			args:    []string{"start", "--runner", "kubernetes"},
			wantErr: `--runner "kubernetes" is not one of`,
		},
		{
			name:    "an unknown runner in the environment is rejected too",
			args:    []string{"start"},
			env:     map[string]string{"CLOUDRIG_RUNNER": "kubernetes"},
			wantErr: "is not one of",
		},
		{
			name:    "a stray positional argument is rejected",
			args:    []string{"start", "extra"},
			wantErr: `unexpected argument "extra"`,
		},
		{
			name:    "an unknown command is rejected",
			args:    []string{"serve"},
			wantErr: `unknown command "serve"`,
		},
		{
			name:    "no command at all",
			args:    nil,
			wantErr: "missing command",
		},
		{
			name:    "an unknown flag is rejected",
			args:    []string{"start", "--nope"},
			wantErr: "flag provided but not defined",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseConfig(tc.args, env(tc.env), io.Discard)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("got config %+v, want error containing %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.port != tc.wantPort {
				t.Errorf("port = %d, want %d", got.port, tc.wantPort)
			}
			if got.runner != tc.wantRunner {
				t.Errorf("runner = %q, want %q", got.runner, tc.wantRunner)
			}
		})
	}
}

func TestParseConfigHelp(t *testing.T) {
	t.Parallel()

	// --help must be distinguishable from a failure, so main can exit 0.
	var out strings.Builder
	_, err := parseConfig([]string{"--help"}, env(nil), &out)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("error = %v, want flag.ErrHelp", err)
	}
	for _, want := range []string{"-port", "-runner", "CLOUDRIG_PORT", "CLOUDRIG_RUNNER"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage does not mention %q:\n%s", want, out.String())
		}
	}
}

func TestConfigAddr(t *testing.T) {
	t.Parallel()

	// Bind every interface: the binary must be reachable from a container.
	if got := (config{port: 4599}).addr(); got != ":4599" {
		t.Errorf("addr() = %q, want %q", got, ":4599")
	}
}

func TestParseFnRun(t *testing.T) {
	t.Parallel()

	const hello = "../../examples/hello"

	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		wantSource string
		wantName   string
		wantEntry  string
		wantPort   int
		wantErr    string
	}{
		{
			name:       "dir before flags",
			args:       []string{"run", hello, "--entry-point", "HelloHTTP", "--port", "5000"},
			wantSource: hello,
			wantName:   "hello",
			wantEntry:  "HelloHTTP",
			wantPort:   5000,
		},
		{
			// flag stops at the first non-flag argument, so this ordering is
			// the one that used to swallow every flag after the directory.
			name:       "dir after flags",
			args:       []string{"run", "--entry-point", "HelloHTTP", hello},
			wantSource: hello,
			wantName:   "hello",
			wantEntry:  "HelloHTTP",
			wantPort:   4599,
		},
		{
			name:       "explicit name",
			args:       []string{"run", hello, "--entry-point", "Echo", "--name", "greeter"},
			wantSource: hello,
			wantName:   "greeter",
			wantEntry:  "Echo",
			wantPort:   4599,
		},
		{
			name:       "port from the environment",
			args:       []string{"run", hello, "--entry-point", "Echo"},
			env:        map[string]string{"CLOUDRIG_PORT": "6000"},
			wantSource: hello,
			wantName:   "hello",
			wantEntry:  "Echo",
			wantPort:   6000,
		},
		{
			// Runtime and entry point now resolve in the functions package, so
			// the CLI passes them through untouched.
			name:       "entry point left for the runtime to resolve",
			args:       []string{"run", hello},
			wantSource: hello,
			wantName:   "hello",
			wantPort:   4599,
		},
		{name: "no source", args: []string{"run"}, wantErr: "needs a source directory"},
		{name: "not the run subcommand", args: []string{"deploy"}, wantErr: "usage: cloudrig fn run"},
		{name: "no subcommand", args: nil, wantErr: "usage: cloudrig fn run"},
		{
			name:    "unexpected extra argument",
			args:    []string{"run", hello, "extra", "--entry-point", "Echo"},
			wantErr: `unexpected argument "extra"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseFnRun(tc.args, env(tc.env), io.Discard)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("got %+v, want error containing %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.source != tc.wantSource {
				t.Errorf("source = %q, want %q", got.source, tc.wantSource)
			}
			if got.name != tc.wantName {
				t.Errorf("name = %q, want %q", got.name, tc.wantName)
			}
			if got.entryPoint != tc.wantEntry {
				t.Errorf("entryPoint = %q, want %q", got.entryPoint, tc.wantEntry)
			}
			if got.port != tc.wantPort {
				t.Errorf("port = %d, want %d", got.port, tc.wantPort)
			}
		})
	}
}
