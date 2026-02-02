.DEFAULT_GOAL := help

GO      ?= go
PKG     := ./...
INTERNAL := ./internal/...
DB_URL  ?= postgres://ledgerd:ledgerd@localhost:5432/ledgerd?sslmode=disable
COVER_MIN := 85

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build both binaries into ./bin
	$(GO) build -o bin/ledgerd ./cmd/ledgerd
	$(GO) build -o bin/workerd ./cmd/workerd

.PHONY: run
run: ## Run the API server
	$(GO) run ./cmd/ledgerd

.PHONY: run-worker
run-worker: ## Run the worker
	$(GO) run ./cmd/workerd

.PHONY: test
test: ## Unit + property tests (no Docker)
	$(GO) test -count=1 $(PKG)

.PHONY: test-race
test-race: ## Unit + property tests under -race
	$(GO) test -race -count=1 $(PKG)

.PHONY: test-integration
test-integration: ## Integration tests against a real Postgres (needs Docker)
	$(GO) test -race -count=1 -tags=integration ./test/integration/...

.PHONY: cover
cover: ## Coverage report over ./internal, gated at $(COVER_MIN)%
	$(GO) test -race -count=1 -coverprofile=cover.out $(INTERNAL)
	$(GO) tool cover -func=cover.out | tail -1

.PHONY: lint
lint: ## go vet + golangci-lint
	$(GO) vet $(PKG)
	golangci-lint run

.PHONY: vuln
vuln: ## govulncheck
	govulncheck $(PKG)

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

.PHONY: sqlc
sqlc: ## Regenerate type-safe query code
	sqlc generate

.PHONY: migrate
migrate: ## Apply migrations
	goose -dir migrations postgres "$(DB_URL)" up

.PHONY: migrate-down
migrate-down: ## Roll back one migration
	goose -dir migrations postgres "$(DB_URL)" down

.PHONY: db-up
db-up: ## Start Postgres 16
	docker compose up -d postgres

.PHONY: db-down
db-down: ## Stop and remove local infrastructure
	docker compose down -v

.PHONY: clean
clean: ## Remove build and coverage artifacts
	rm -rf bin cover.out coverage.html
