BASE_DIR = $(realpath .)
SHELL = bash

all: build

.prep: ## Prepare
	poetry lock
	poetry install --no-root
	touch .prep

.PHONY: lint
lint: .prep ## Run python lint
	poetry lock
	poetry install --no-root
	poetry run isort cairn_sidecar tests
	poetry run ruff format cairn_sidecar tests
	poetry run ruff check cairn_sidecar tests
	time poetry run pylint cairn_sidecar tests --min-similarity-lines=30
	poetry run mypy -p cairn_sidecar --follow-imports skip --check-untyped-defs

.PHONY: build
build: lint ## Build module
	poetry build
	poetry export -f requirements.txt > requirements.txt

.PHONY: test
test: ## Run unit-tests
	poetry run pytest --verbose --junitxml=test-reports/test.xml tests/

.PHONY: one-test
one-test: ## Run specific unit-tests
	poetry run pytest -vv --tb=long -s -k "$(FILTER)" tests/

# .PHONY: install
install: build ## Install module
	poetry install

.PHONY: docker
docker: ## Build side cart dev docker image
	docker build -t "alwitt/cairn-sidecar:latest" -f Dockerfile .

.PHONY: cli
cli: .prep ## Run venv python CLI
	poetry run python3

.PHONY: clean
clean: ## Clean up the python build artifacts
	rm -rf venv .venv .prep build dist .pytest_cache test-reports

help: ## Display this help screen
	@grep -h -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'
