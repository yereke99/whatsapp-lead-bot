# Development helpers and the systemd deployment targets.

BINARY_DIR   := bin
GO           ?= go
SERVICE_NAME ?= whatsapp
APP_DIR      := $(shell pwd)

# Every target that touches the database reads .env the way the service does.
RUN_ENV = set -a && . ./.env && set +a &&

.PHONY: help run build test test-integration test-all test-race cover fmt vet lint tidy \
        migrate seed clean install-service start stop restart status logs \
        docker-build docker-up docker-down

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# ------------------------------------------------------------ development --

run: ## Run the whole system locally with the local .env
	@test -f .env || { echo "no .env found; run: cp .env.example .env"; exit 1; }
	$(RUN_ENV) $(GO) run ./cmd/server

build: ## Compile all binaries into ./bin
	$(GO) build -trimpath -o $(BINARY_DIR)/server  ./cmd/server
	$(GO) build -trimpath -o $(BINARY_DIR)/migrate ./cmd/migrate
	$(GO) build -trimpath -o $(BINARY_DIR)/seed    ./cmd/seed

migrate: ## Apply database migrations
	$(RUN_ENV) $(GO) run ./cmd/migrate

seed: ## Load the example Ayran webinar campaign
	$(RUN_ENV) $(GO) run ./cmd/seed

# ----------------------------------------------------------------- tests --

test: ## Run unit tests
	$(GO) test ./...

test-integration: ## Run integration tests (uses a scratch SQLite file)
	$(GO) test -tags=integration ./internal/integration/...

test-all: test test-integration ## Run every test

test-race: ## Run every test under the race detector
	$(GO) test -race ./...
	$(GO) test -race -tags=integration ./internal/integration/...

cover: ## Report test coverage
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

fmt: ## Format the source tree
	gofmt -w cmd internal pkg

vet: ## Run go vet
	$(GO) vet ./...
	$(GO) vet -tags=integration ./...

lint: fmt vet ## Format and vet

tidy: ## Tidy module dependencies
	$(GO) mod tidy

clean: ## Remove build artefacts
	rm -rf $(BINARY_DIR) coverage.out

# ------------------------------------------------------------ deployment --

install-service: ## Write /etc/systemd/system/$(SERVICE_NAME).service if absent (never overwrites)
	@SERVICE_NAME=$(SERVICE_NAME) APP_DIR=$(APP_DIR) sudo -E ./deploy/install-service.sh

start: install-service ## Install the unit if needed, then start the system
	sudo systemctl start $(SERVICE_NAME).service
	@sleep 1
	@systemctl status $(SERVICE_NAME).service --no-pager || true

stop: ## Stop the system
	sudo systemctl stop $(SERVICE_NAME).service

restart: ## Restart the system
	sudo systemctl restart $(SERVICE_NAME).service
	@sleep 1
	@systemctl status $(SERVICE_NAME).service --no-pager || true

status: ## Show service status
	@systemctl status $(SERVICE_NAME).service --no-pager || true

logs: ## Follow service logs
	journalctl -u $(SERVICE_NAME).service -f

# ---------------------------------------------------------------- docker --

docker-build: ## Build the container image
	docker compose build

docker-up: ## Start the stack
	docker compose up -d

docker-down: ## Stop the stack
	docker compose down
