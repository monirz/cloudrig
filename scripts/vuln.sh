#!/bin/sh
# Fails on any vulnerability this module can actually reach, so a finding is a
# call path rather than a version number in the dependency graph.
set -eu

# Pinned, not @latest: CI must run the reviewed scanner, not whatever ships
# next. Bump this deliberately and let the change go through review.
GOVULNCHECK=golang.org/x/vuln/cmd/govulncheck@v1.8.0

# GOWORK=off because a local go.work shadows the module's toolchain line, and
# the toolchain decides which standard library is scanned.
GOWORK=off go run "$GOVULNCHECK" ./...
