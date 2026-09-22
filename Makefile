.DEFAULT_GOAL := help

REDIS_ADDR ?= localhost:6379
ADDR       ?= :8080

.PHONY: help up down run test test-integration bench lint fmt tidy check

help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk -F':.*?## ' '{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

up: ## Start Redis
	docker compose up -d --wait

down: ## Stop Redis and remove its data
	docker compose down -v

run: ## Run the server (needs: make up)
	REDIS_ADDR=$(REDIS_ADDR) ADDR=$(ADDR) go run ./cmd/server

test: ## Unit tests (miniredis, no Docker needed)
	go test -race ./...

test-integration: ## Integration tests against real Redis (needs: make up)
	REDIS_ADDR=$(REDIS_ADDR) go test -race -tags=integration ./...

bench: ## Benchmark AllowN: latency and allocations
	go test -bench=. -benchmem -run='^$$' ./internal/limiter/...

lint: ## Run golangci-lint
	@command -v golangci-lint >/dev/null || { \
		echo "golangci-lint not installed:"; \
		echo "  brew install golangci-lint"; exit 1; }
	golangci-lint run

fmt: ## Format and fix imports
	go fmt ./...

tidy: ## Sync go.mod and go.sum
	go mod tidy

check: fmt tidy test ## Everything that must pass before a commit
