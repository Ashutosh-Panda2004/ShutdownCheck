GO      ?= go
BINARY  := shutdowncheck
PKGS    := ./...

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)
FUZZTIME ?= 30s

LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

.PHONY: help build test race cover lint vet fmt tidy vuln fuzz docs snapshot release-check ci clean

help: ## Show available targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-8s %s\n", $$1, $$2}'

build: ## Build the binary into bin/
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/shutdowncheck

test: ## Run unit tests
	$(GO) test $(PKGS)

race: ## Run tests under the race detector
	$(GO) test -race $(PKGS)

cover: ## Run tests with coverage and print the total
	$(GO) test -covermode=atomic -coverprofile=coverage.out $(PKGS)
	@$(GO) tool cover -func=coverage.out | tail -n 1

fmt: ## Check formatting
	@unformatted=$$(gofmt -l . | grep -v '^test/conformance/' || true); \
	if [ -n "$$unformatted" ]; then echo "not gofmt'd:"; echo "$$unformatted"; exit 1; fi

vet: ## Run go vet
	$(GO) vet $(PKGS)

lint: fmt vet ## Run formatting, vet and golangci-lint if available
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed, skipping (see CONTRIBUTING.md)"; \
	fi

tidy: ## Verify go.mod is tidy
	$(GO) mod tidy
	@git diff --quiet -- go.mod go.sum || (echo "go.mod/go.sum are not tidy; commit the result of 'go mod tidy'"; exit 1)

vuln: ## Check dependencies for known vulnerabilities
	@if command -v govulncheck >/dev/null 2>&1; then \
		govulncheck $(PKGS); \
	else \
		echo "govulncheck not installed: go install golang.org/x/vuln/cmd/govulncheck@v1.7.0"; \
	fi

fuzz: ## Run each fuzz target for FUZZTIME (default 30s)
	$(GO) test ./internal/timeline -run=XXX -fuzz=FuzzReadNDJSON -fuzztime=$(FUZZTIME)
	$(GO) test ./internal/config -run=XXX -fuzz=FuzzParse -fuzztime=$(FUZZTIME)

docs: ## Regenerate the published signature pages from the catalogue
	$(GO) test ./internal/remediate -update
	@git diff --quiet -- docs/signatures || echo "docs/signatures updated; commit the result"

release-check: ## Validate .goreleaser.yaml
	@command -v goreleaser >/dev/null 2>&1 \
		|| { echo "goreleaser not installed: https://goreleaser.com/install"; exit 1; }
	goreleaser check

snapshot: ## Build release artefacts locally without publishing anything
	@command -v goreleaser >/dev/null 2>&1 \
		|| { echo "goreleaser not installed: https://goreleaser.com/install"; exit 1; }
	goreleaser build --snapshot --clean

ci: lint test race cover ## Everything CI runs

clean: ## Remove build artefacts
	rm -rf bin dist coverage.out
