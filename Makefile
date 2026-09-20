GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.DEFAULT_GOAL := help
.PHONY: help all build demo examples tests test-unit test-integration coverage vet lint fmt fmt-check tidy vuln release-check check clean

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

examples: ## Build the nested example and fixture modules
	@for m in $$(find examples testdata -name go.mod -not -path '*/node_modules/*' -exec dirname {} \;); do \
		echo "  $$m"; (cd $$m && GOWORK=off $(GO) build ./...) || exit 1; \
	done

# The tape drives the built binary through PATH, so build first. vhs captures
# the frames; ffmpeg encodes them, because vhs's own GIF step is broken against
# current ffmpeg and fails without saying so.
DEMO_FRAMES := .demoframes
DEMO_FILTER := fps=14,scale=900:-1:flags=lanczos,split[a][b];\
[a]palettegen=max_colors=192:stats_mode=diff[p];\
[b][p]paletteuse=dither=bayer:bayer_scale=4:diff_mode=rectangle

demo: build ## Re-record assets/demo.gif (needs vhs and ffmpeg)
	@rm -rf $(DEMO_FRAMES)
	vhs assets/demo.tape
	ffmpeg -y -loglevel error -framerate 50 -i '$(DEMO_FRAMES)/frame-text-%05d.png' \
		-filter_complex "$(DEMO_FILTER)" -loop 0 assets/demo.gif
	@rm -rf $(DEMO_FRAMES)
	@echo "  assets/demo.gif: $$(du -h assets/demo.gif | cut -f1)"

## Quality

vet: ## go vet
	$(GO) vet ./...

GOLANGCI_VERSION ?= v2.13.2
# The official binary, not go run: upstream does not support go-installed builds.
GOLANGCI := bin/golangci-lint-$(GOLANGCI_VERSION)

$(GOLANGCI):
	@mkdir -p bin
	curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b bin $(GOLANGCI_VERSION)
	@mv bin/golangci-lint $@

# The timer check is a go test over go/ast: go vet has no such analyzer.
lint: $(GOLANGCI) ## golangci-lint and the timer lint
	$(GOLANGCI) run
	$(GO) test ./lint/

fmt: $(GOLANGCI) ## Apply gofumpt and import grouping
	$(GOLANGCI) fmt

tidy: ## go mod tidy
	$(GO) mod tidy

# GOWORK=off inside the script: a local go.work would shadow the module's toolchain.
vuln: ## govulncheck, reachable paths only
	@GO=$(GO) sh scripts/vuln.sh

# Verifies, never rewrites: a formatting fix is the author's commit, not CI's diff.
fmt-check: $(GOLANGCI) ## Fail on unformatted files or ungrouped imports
	$(GOLANGCI) fmt --diff

release-check: ## Validate .goreleaser.yaml
	$(GO) run github.com/goreleaser/goreleaser/v2@v2.18.2 check

check: vet lint fmt-check build examples tests ## What CI runs: vet, lint, format, tidy, build, race tests
	$(GO) mod tidy -diff
	$(GO) build ./...

clean: ## Remove the binary, coverage output and test cache
	rm -f cloudrig coverage.out
	$(GO) clean -testcache
