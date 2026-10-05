# dashd developer tasks.
#
# `make check` is the gate: everything CI runs, locally, in one command.
# Run it before pushing.

GO      ?= go
BINARY  ?= dashd
PKG     := ./cmd/dashd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# Stamped only when building something meant to be kept. A plain `make build`
# stays fast and does not force a git invocation.
LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

IMAGE ?= dashd:dev

# staticcheck runs with two checks disabled. Both are temporary and both are
# real findings in code this file's owner does not touch; they are listed here
# so they cannot be forgotten. Remove each line once the Go change lands.
#
#   -U1000  unused code, currently:
#             internal/web/activity.go:allowedSteps, isAllowedStep
#             internal/web/render.go:shortPath
#             internal/web/web_test.go:newTestStoreWithCall
#   -S1011  a loop in internal/store/queries.go:42 that appends in a row;
#           `clauses = append(clauses, bounds...)` is the same thing.
#
# Everything else, including every correctness check, is enforced.
STATICCHECK_CHECKS := -checks=inherit,-U1000,-S1011

.DEFAULT_GOAL := help
.PHONY: help build test test-race cover vet fmt fmt-check lint check run scan install docker-build clean

help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build: ## Build the binary into ./dashd
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

test: ## Run the tests
	$(GO) test ./...

test-race: ## Run the tests under the race detector
	$(GO) test -race ./...

cover: ## Run the tests and report total coverage
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## Rewrite files with gofmt
	gofmt -w .

fmt-check: ## Fail if anything is unformatted (CI runs this)
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt found unformatted files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

lint: ## Run staticcheck
	$(GO) run honnef.co/go/tools/cmd/staticcheck@latest $(STATICCHECK_CHECKS) ./...

# The gate. This is what CI enforces and what you should run before pushing.
check: fmt-check vet lint test ## Format check, vet, staticcheck and tests
	@echo "ok"

run: build ## Build and serve the dashboard
	./$(BINARY) serve-and-scan

scan: build ## Build and run a single scan, then print stats
	./$(BINARY) scan

install: ## Install the binary into GOBIN (or GOPATH/bin)
	CGO_ENABLED=0 $(GO) install -trimpath -ldflags "$(LDFLAGS)" $(PKG)

docker-build: ## Build the container image
	docker build -t $(IMAGE) .

clean: ## Remove build output
	rm -f $(BINARY) coverage.out coverage.html
	rm -rf dist