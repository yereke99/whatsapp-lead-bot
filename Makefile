# Development helpers and the systemd deployment targets.

BINARY_DIR   := bin
GO           ?= go
SERVICE_NAME ?= whatsapp
APP_DIR      := $(shell pwd)

# Every target that touches the database reads .env the way the service does.
RUN_ENV = set -a && . ./.env && set +a &&

RUN_DIR  := run
PID_FILE := $(RUN_DIR)/server.pid
LOG_FILE := $(RUN_DIR)/server.log

.PHONY: help run dev run-stop run-restart run-status run-logs build \
        test test-integration test-all test-race cover fmt vet lint tidy \
        migrate seed clean install-service start stop restart status logs \
        docker-build docker-up docker-down

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# ------------------------------------------------------------ development --

# The server is compiled first and the binary is what gets backgrounded.
# Backgrounding `go run` instead would record the pid of its wrapper process
# rather than the server's, so stopping it could leave the server orphaned and
# still holding the port.
run: ## Start the system in the background (logs to run/server.log)
	@test -f .env || { echo "no .env found; run: cp .env.example .env"; exit 1; }
	@if [ -f "$(PID_FILE)" ] && kill -0 "$$(cat $(PID_FILE))" 2>/dev/null; then \
		echo "already running (pid $$(cat $(PID_FILE))); use 'make run-restart'"; exit 1; \
	fi
	@mkdir -p $(RUN_DIR)
	@$(GO) build -trimpath -o $(BINARY_DIR)/server ./cmd/server
	@set -a; . ./.env; set +a; nohup $(BINARY_DIR)/server >> $(LOG_FILE) 2>&1 & echo $$! > $(PID_FILE)
	@sleep 2
	@if kill -0 "$$(cat $(PID_FILE))" 2>/dev/null; then \
		echo "started in background (pid $$(cat $(PID_FILE)))"; \
		echo "  logs:  make run-logs"; \
		echo "  stop:  make run-stop"; \
	else \
		rm -f $(PID_FILE); \
		echo "failed to start; last lines of $(LOG_FILE):"; tail -n 20 $(LOG_FILE); exit 1; \
	fi

dev: ## Run in the foreground, for development (Ctrl-C to stop)
	@test -f .env || { echo "no .env found; run: cp .env.example .env"; exit 1; }
	$(RUN_ENV) $(GO) run ./cmd/server

run-stop: ## Stop the backgrounded system
	@if [ ! -f "$(PID_FILE)" ]; then echo "not running (no $(PID_FILE))"; exit 0; fi
	@pid=$$(cat $(PID_FILE)); \
	if kill -0 "$$pid" 2>/dev/null; then \
		kill -INT "$$pid"; \
		for i in 1 2 3 4 5 6 7 8 9 10; do \
			kill -0 "$$pid" 2>/dev/null || break; sleep 1; \
		done; \
		kill -0 "$$pid" 2>/dev/null && kill -KILL "$$pid" || true; \
		echo "stopped (pid $$pid)"; \
	else \
		echo "not running (stale pid $$pid)"; \
	fi; \
	rm -f $(PID_FILE)

run-restart: run-stop run ## Restart the backgrounded system

run-status: ## Show whether the backgrounded system is running
	@if [ -f "$(PID_FILE)" ] && kill -0 "$$(cat $(PID_FILE))" 2>/dev/null; then \
		echo "running (pid $$(cat $(PID_FILE)))"; \
	else \
		echo "not running"; \
	fi

run-logs: ## Follow the backgrounded system's logs
	@touch $(LOG_FILE); tail -f $(LOG_FILE)

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
	rm -rf $(BINARY_DIR) $(RUN_DIR) coverage.out

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
