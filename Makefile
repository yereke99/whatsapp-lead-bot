# Development helpers. Production deployment uses docker compose.

BINARY_DIR := bin
GO         ?= go

.PHONY: help build run test test-integration test-all lint fmt vet tidy migrate seed docker-build docker-up docker-down clean

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

build: ## Compile all binaries into ./bin
	$(GO) build -trimpath -o $(BINARY_DIR)/server  ./cmd/server
	$(GO) build -trimpath -o $(BINARY_DIR)/migrate ./cmd/migrate
	$(GO) build -trimpath -o $(BINARY_DIR)/seed    ./cmd/seed

run: ## Run the server with the local .env
	set -a && . ./.env && set +a && $(GO) run ./cmd/server

test: ## Run unit tests
	$(GO) test ./...

test-integration: ## Run integration tests (needs TEST_DATABASE_URL)
	$(GO) test -tags=integration ./internal/integration/...

test-all: test test-integration ## Run every test

test-race: ## Run unit tests under the race detector
	$(GO) test -race ./...

cover: ## Report test coverage
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

fmt: ## Format the source tree
	gofmt -w cmd internal pkg

vet: ## Run go vet
	$(GO) vet ./...

lint: fmt vet ## Format and vet

tidy: ## Tidy module dependencies
	$(GO) mod tidy

migrate: ## Apply database migrations
	set -a && . ./.env && set +a && $(GO) run ./cmd/migrate

seed: ## Load the example Ayran webinar campaign
	set -a && . ./.env && set +a && $(GO) run ./cmd/seed

docker-build: ## Build the container image
	docker compose build

docker-up: ## Start the full stack
	docker compose up -d

docker-down: ## Stop the stack
	docker compose down

clean: ## Remove build artefacts
	rm -rf $(BINARY_DIR) coverage.out
