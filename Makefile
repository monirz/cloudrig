GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build test vet lint fmt check clean

all: check

# Stamps the version from git so `cloudrig start` reports something real.
build:
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o cloudrig ./cmd/cloudrig

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

# The timer check is a go test over go/ast rather than a vet analyzer or a
# golangci-lint config: go vet ships nothing for it, and forbidigo would be a
# dependency for eighty lines of walk. `make test` runs it too; this target
# exists so CI and a developer can ask for it by name.
lint:
	$(GO) test ./lint/

fmt:
	$(GO) run mvdan.cc/gofumpt@latest -l -w . 2>/dev/null || gofmt -l -w .

# check is what CI runs. gofmt is enforced rather than applied, so a formatting
# fix is a commit the author made, not a diff CI leaves behind.
# Builds the binary too: a stale ./cloudrig is an easy way to test the wrong
# code by hand.
check: vet lint build
	$(GO) build ./...
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt'd:"; echo "$$unformatted"; exit 1; \
	fi
	$(GO) test -race ./...

clean:
	rm -f cloudrig
	$(GO) clean -testcache
