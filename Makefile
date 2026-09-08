# IFA
#
# `make demo` is the one target worth knowing: it builds the control plane and
# runs it against a simulated inference fleet, so you can see what the project
# does without a cluster, a GPU, or a vLLM deployment.

SHELL := /bin/bash
.DEFAULT_GOAL := help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GO_PACKAGES := ./...
BIN := bin
CHART := deploy/helm/ifa
IMAGE ?= ghcr.io/p95labs/ifa
IMAGE_TAG ?= $(VERSION)

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the control plane and the ifa CLI into ./bin
	@mkdir -p $(BIN)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/control-plane ./cmd/control-plane
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/ifa ./cmd/ifa

.PHONY: demo
demo: build ## Run the control plane against a simulated inference fleet
	@./scripts/demo.sh

.PHONY: run
run: build ## Run the control plane against the targets in config.yaml
	@$(BIN)/control-plane -config config.yaml

.PHONY: test
test: ## Run all tests
	go test -count=1 $(GO_PACKAGES)

.PHONY: test-race
test-race: ## Run all tests under the race detector
	go test -race -count=1 $(GO_PACKAGES)

.PHONY: cover
cover: ## Run tests with coverage and write coverage.html
	go test -coverprofile=coverage.out -covermode=atomic $(GO_PACKAGES)
	go tool cover -func=coverage.out | tail -1
	go tool cover -html=coverage.out -o coverage.html
	@echo "wrote coverage.html"

.PHONY: bench
bench: ## Run benchmarks for the parsing and rule-evaluation hot paths
	go test -run '^$$' -bench . -benchmem ./internal/promtext/... ./internal/runtime/... ./internal/recommender/...

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -w -s $(shell git ls-files '*.go')

.PHONY: lint
lint: ## Check formatting and run go vet
	@unformatted=$$(gofmt -l -s $$(git ls-files '*.go')); \
	if [ -n "$$unformatted" ]; then \
		echo "these files are not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi
	go vet $(GO_PACKAGES)

.PHONY: tidy
tidy: ## Tidy go.mod and fail if it changed
	go mod tidy
	@git diff --exit-code go.mod go.sum

.PHONY: helm-lint
helm-lint: ## Lint and render the Helm chart
	helm lint $(CHART)
	helm template ifa $(CHART) >/dev/null
# NetworkPolicy, the PDB and the ServiceMonitor are all off by default, so the
# render above never reaches them. Without this second pass a syntax error in
# any of the three ships undetected and only surfaces for the operator who
# turns the feature on.
	helm template ifa $(CHART) \
		--set networkPolicy.enabled=true \
		--set networkPolicy.scrapeNamespaces={inference} \
		--set networkPolicy.dcgmNamespaces={gpu-operator} \
		--set networkPolicy.databaseNamespace=timescale \
		--set podDisruptionBudget.enabled=true \
		--set replicaCount=2 \
		--set serviceMonitor.enabled=true >/dev/null

.PHONY: docker
docker: ## Build the container image
	docker build -f deploy/docker/Dockerfile -t $(IMAGE):$(IMAGE_TAG) .

.PHONY: verify
verify: lint test-race helm-lint ## Everything CI runs

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN) coverage.out coverage.html
