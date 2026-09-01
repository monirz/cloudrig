#!/bin/sh
# Builds the demo image. Run it from this directory:
#
#   ./build.sh            -> cloudrun-demo:latest
#
# The binary is compiled for the Docker daemon's architecture, not this
# machine's: they differ whenever the daemon is a VM, and a mismatch shows up
# as an exec format error inside the container rather than at build time.
set -e

reported=$(docker info --format '{{.Architecture}}')
case "$reported" in
	aarch64|arm64) arch=arm64 ;;
	x86_64|amd64)  arch=amd64 ;;
	armv7l|armv6l) arch=arm ;;
	*)             arch=$reported ;;
esac

echo "building for linux/$arch (daemon reports $reported)"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" GOWORK=off go build -o server .
docker build -t cloudrun-demo:latest .
rm -f server
echo "built cloudrun-demo:latest"
