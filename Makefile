LATEST_COMMIT_SHORT_SHA = $$(git rev-parse --short HEAD)
BASE_DIR = $(realpath .)
SHELL = bash

include .env
export

all: build

.PHONY: lint
lint: .prepare ## Lint the files
	@go mod tidy
	@go fmt ./...
	@go vet ./...
	@revive ./...
	@golangci-lint run ./...

.PHONY: fix
fix: .prepare ## Lint and fix violations
	@go mod tidy
	@go fmt ./...
	@go vet ./...
	@revive ./...
	@golangci-lint run --fix ./...

.PHONY: build
build: lint ## Build application
	@CGO_ENABLED=0 go build -ldflags "-s -w" -o cairn .

.PHONY: test
test: .prepare ## Run unit tests
	go test --count 1 -timeout 60s -short ./...

.PHONY: one-test
one-test: .prepare ## Run one unittest. Set `FILTER` as target test
	go test --count 1 -v -timeout 60s -run ^$(FILTER)$$ github.com/alwitt/cairn/...

.PHONY: test-package
test-package: .prepare ## Run all tests in a package. Set `PKG` as target package
	go test --count 1 -timeout 60s -short github.com/alwitt/cairn/$(PKG)/...

.PHONY: mock
mock: ## Define support mocks
	@mockery

.PHONY: up
up: .prepare ## Start docker compose development stack
	docker compose -f docker/docker-compose.yml up -d

.PHONY: down
down: .prepare ## Stop docker compose development stack
	docker compose -f docker/docker-compose.yml down

.PHONY: gen-migrate
gen-migrate: ## Define new database migration
	atlas migrate diff \
	  --env gorm \
	  --format '{{ sql . "  " }}'

.PHONY: dev-migrate
dev-migrate: ## Test apply database migration to DEV Postgres
	atlas migrate apply \
	  --env gorm \
	  --url "postgres://cairn:cairn@localhost:6532/postgres?search_path=public&sslmode=disable"

.PHONY: api
api: build ## Start local development API server
	. .env && ./cairn server -c demo/server_config.yml

.prepare: ## Prepare the project for local development
	@pip3 install pre-commit
	@pre-commit install
	@pre-commit install-hooks
	@touch .prepare

help: ## Display this help screen
	@grep -h -E '^[a-z0-9A-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'
