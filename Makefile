GO ?= go

.PHONY: all build test vet lint fmt check clean

all: check

build:
	$(GO) build ./...

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
check: build vet lint
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt'd:"; echo "$$unformatted"; exit 1; \
	fi
	$(GO) test -race ./...

clean:
	$(GO) clean -testcache
