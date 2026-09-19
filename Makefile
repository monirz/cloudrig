GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.DEFAULT_GOAL := help
.PHONY: help all build tests test-unit test-integration coverage vet lint fmt fmt-check tidy vuln check clean

help: ## List targets
	@grep -hE '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-17s %s\n", $$1, $$2}'

all: check

## Build

build: ## Build ./cloudrig with the git version stamped in
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o cloudrig ./cmd/cloudrig

## Test

tests: ## Full suite with -race: unit and integration
	$(GO) test -race ./...

test-unit: ## Fast tests only; skips what checks testing.Short()
	$(GO) test -race -short ./...

# Go cannot select only the tests -short skips, so this is the full suite.
test-integration: tests ## Docker, gcloud and compile-heavy tests (full suite)

coverage: ## coverage.out over product code, examples excluded
	$(GO) test ./... -covermode=atomic -coverprofile=coverage.out \
		-coverpkg="$$($(GO) list ./... | grep -v /examples | paste -sd, -)"

## Quality

vet: ## go vet
	$(GO) vet ./...

# A go test over go/ast: go vet has no timer check, and forbidigo is not worth a dependency.
lint: ## Timer lint
	$(GO) test ./lint/

fmt: ## Apply gofumpt, falling back to gofmt
	$(GO) run mvdan.cc/gofumpt@latest -l -w . 2>/dev/null || gofmt -l -w .

tidy: ## go mod tidy
	$(GO) mod tidy

# GOWORK=off inside the script: a local go.work would shadow the module's toolchain.
vuln: ## govulncheck, reachable paths only
	@GO=$(GO) sh scripts/vuln.sh

# Verifies, never rewrites: a formatting fix is the author's commit, not CI's diff.
fmt-check: ## Fail if any file is not gofmt'd
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt'd:"; echo "$$unformatted"; exit 1; \
	fi

check: vet lint fmt-check build tests ## What CI runs: vet, lint, gofmt, tidy, build, race tests
	$(GO) mod tidy -diff
	$(GO) build ./...

clean: ## Remove the binary, coverage output and test cache
	rm -f cloudrig coverage.out
	$(GO) clean -testcache
