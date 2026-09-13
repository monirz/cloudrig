# Contributing

Contributions, bug reports and ideas are welcome.

## Build and test

Requires Go 1.25+.

```sh
git clone https://github.com/monirz/cloudrig && cd cloudrig
make build              # -> ./cloudrig
make check              # build, vet, lint, gofmt, race tests
make vuln               # govulncheck, reachable paths only
```

Conformance tests drive the real `cloud.google.com/go` clients and the `gcloud`
binary (skipped when gcloud is not installed).

## Conventions

- **One PR per feature or service.** Keep changes reviewable.
- **gRPC and REST together.** gcloud/Terraform compatibility is a priority, so a
  service ships both surfaces, not gRPC-only.
- **No stubs that lie.** Emulate honestly; document every gap in
  [UNSUPPORTED.md](UNSUPPORTED.md).
- **Time is injected.** Read the clock through `core/clock`, never the wall
  clock, so tests stay deterministic.

## Architecture

[ARCHITECTURE.md](ARCHITECTURE.md) explains how the one-port transport, the
service layer and the injected clock fit together.
