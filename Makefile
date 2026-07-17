# omnicore — developer task runner.
#
# Every build links TWO mandatory build tags: a relational engine
# (postgres|mysql|sqlserver|oracle) AND a message transport (kafka|nats). There is no default on
# either axis — a tagless build aborts at boot. These targets encapsulate the
# tag matrix so `make test` matches CI and nobody has to memorize the flags.
#
# Override the active tag pair on any target:  make test TAGS='mysql nats'
#
# NOTE: there is deliberately no `tidy` target — `go mod tidy` prunes the
# tag-gated engine/transport deps and breaks the build (see CLAUDE.md).

TAGS ?= postgres kafka
GO   ?= go
PKGS ?= ./...

# The full engine×transport matrix CI exercises.
MATRIX := 'postgres kafka' 'mysql kafka' 'sqlserver kafka' 'oracle kafka' 'postgres nats' 'mysql nats' 'sqlserver nats' 'oracle nats'

.PHONY: help
help: ## List available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build with $(TAGS)
	$(GO) build -tags '$(TAGS)' $(PKGS)

.PHONY: vet
vet: ## go vet with $(TAGS)
	$(GO) vet -tags '$(TAGS)' $(PKGS)

.PHONY: test
test: ## Unit tests with $(TAGS)
	$(GO) test -tags '$(TAGS)' $(PKGS) -count=1

.PHONY: test-race
test-race: ## Unit tests with the race detector
	$(GO) test -tags '$(TAGS)' $(PKGS) -count=1 -race

.PHONY: cover
cover: ## Unit tests + total coverage summary
	$(GO) test -tags '$(TAGS)' $(PKGS) -count=1 -coverprofile=coverage.out
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: matrix
matrix: ## build+vet+test across the full engine×transport matrix
	@for t in $(MATRIX); do \
		echo "=== tags: $$t ==="; \
		$(GO) build -tags "$$t" $(PKGS) || exit 1; \
		$(GO) vet   -tags "$$t" $(PKGS) || exit 1; \
		$(GO) test  -tags "$$t" $(PKGS) -count=1 || exit 1; \
	done

.PHONY: integration
integration: ## Integration tests (needs docker compose up in ../omnicore-example-users/devops)
	$(GO) test -tags 'integration $(TAGS)' $(PKGS) -count=1

.PHONY: lint
lint: ## Run golangci-lint with $(TAGS) (install: make tools)
	golangci-lint run --build-tags '$(TAGS)'

.PHONY: tools
tools: ## Install the pinned dev tools (golangci-lint)
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

.PHONY: fmt
fmt: ## gofmt -w the tree
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if any file needs gofmt
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: check
check: fmt-check vet test ## The pre-push gate: fmt-check + vet + test
