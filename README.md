# cloudrig

A local emulator for Google Cloud APIs. Runs as a binary, or in-process inside a
Go test — no Docker, no daemon.

Requires Go 1.25+.

## Run

```sh
go run ./cmd/cloudrig start                  # :4599
go run ./cmd/cloudrig start --port 5000
CLOUDRIG_PORT=5000 go run ./cmd/cloudrig start
go run ./cmd/cloudrig --help
```

```sh
curl -s localhost:4599/_emu/health
curl -s --http2-prior-knowledge localhost:4599/_emu/health
```

## Use in a test

```go
func TestUpload(t *testing.T) {
	t.Parallel()
	emu := cloudrig.MustStart(t)

	emu.BaseURL()                        // http://127.0.0.1:53412
	emu.FakeClock(t).Advance(time.Hour)  // deterministic, never sleeps
}
```

Each `MustStart` is an isolated instance on its own port. Shutdown is registered
with `t.Cleanup`.

## Test

```sh
make check          # build, vet, timer lint, gofmt, go test -race
make test           # go test -race ./...
make lint           # the timer lint rule alone
go test -short ./...   # skips the tests that build and run the binary
```

## Build

```sh
make build
go build -ldflags "-X main.version=$(git describe --tags)" -o cloudrig ./cmd/cloudrig
```

## Status

Milestone 1: transport, clock, errors, library entry point. No services yet —
`/_emu/health` is the only endpoint, and gRPC returns 501.
