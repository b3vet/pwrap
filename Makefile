SHELL := /bin/bash
.DEFAULT_GOAL := help

BIN := bin
PKG := github.com/b3vet/pwrap

# Injected into `pwrap version`. Without this the CLI reports its 0.0.0-dev
# placeholder even in a tagged build.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: tidy
tidy: ## go mod tidy in all modules
	go mod tidy
	@if [ -f sdk/go/go.mod ]; then cd sdk/go && go mod tidy; fi

.PHONY: build
build: ## build pwrapd + pwrap + example binaries
	mkdir -p $(BIN)
	go build -ldflags "$(LDFLAGS)" -o $(BIN)/pwrapd ./cmd/pwrapd
	go build -ldflags "$(LDFLAGS)" -o $(BIN)/pwrap  ./cmd/pwrap
	go build -o $(BIN)/todo-plus       ./examples/todo-plus
	go build -o $(BIN)/rls-notes       ./examples/rls-notes
	go build -o $(BIN)/geo-spots       ./examples/geo-spots
	go build -o $(BIN)/branching       ./examples/branching
	go build -o $(BIN)/realtime-todos  ./examples/realtime-todos
	go build -o $(BIN)/realtime_smoke  ./examples/realtime_smoke
	go build -o $(BIN)/bench           ./examples/bench

.PHONY: e2e
e2e: build ## reset DB, start pwrapd, run the dogfood demo
	@PWRAP_DATABASE_URL="$${PWRAP_DATABASE_URL:-postgres://pwrap:pwrap@localhost:5432/pwrap?sslmode=disable}" \
	 PWRAP_BOOTSTRAP_TOKEN="$${PWRAP_BOOTSTRAP_TOKEN:-dev-admin}" \
	 PWRAP_AUTHENTICATOR_PASSWORD="$${PWRAP_AUTHENTICATOR_PASSWORD:-authpw-dev}" \
	 PWRAP_JWT_SECRET="$${PWRAP_JWT_SECRET:-super-dev-jwt-secret-please-change-me-32bytes}" \
	 bash scripts/e2e.sh todo-plus

.PHONY: e2e-branching
e2e-branching: build ## run the Track C branching demo
	@PWRAP_DATABASE_URL="$${PWRAP_DATABASE_URL:-postgres://pwrap:pwrap@localhost:5432/pwrap?sslmode=disable}" \
	 PWRAP_BOOTSTRAP_TOKEN="$${PWRAP_BOOTSTRAP_TOKEN:-dev-admin}" \
	 PWRAP_AUTHENTICATOR_PASSWORD="$${PWRAP_AUTHENTICATOR_PASSWORD:-authpw-dev}" \
	 PWRAP_JWT_SECRET="$${PWRAP_JWT_SECRET:-super-dev-jwt-secret-please-change-me-32bytes}" \
	 bash scripts/e2e.sh branching

.PHONY: py-test
py-test: ## run the Python SDK pytest smoke (requires a live pwrapd)
	@if [ ! -d sdk/py/.venv ] && [ ! -f /tmp/pwrap-venv/bin/python ]; then \
		python3 -m venv /tmp/pwrap-venv && \
		/tmp/pwrap-venv/bin/pip install -e sdk/py pytest pytest-asyncio -q; \
	fi
	@cd sdk/py && /tmp/pwrap-venv/bin/pytest -q

.PHONY: e2e-realtime
e2e-realtime: build ## start the realtime-todos web demo at http://localhost:7799
	@PWRAP_BOOTSTRAP_TOKEN="$${PWRAP_BOOTSTRAP_TOKEN:-dev-admin}" \
	 ./bin/realtime-todos

.PHONY: e2e-geo
e2e-geo: build ## run the Track B geo + partitioning demo
	@PWRAP_DATABASE_URL="$${PWRAP_DATABASE_URL:-postgres://pwrap:pwrap@localhost:5432/pwrap?sslmode=disable}" \
	 PWRAP_BOOTSTRAP_TOKEN="$${PWRAP_BOOTSTRAP_TOKEN:-dev-admin}" \
	 PWRAP_AUTHENTICATOR_PASSWORD="$${PWRAP_AUTHENTICATOR_PASSWORD:-authpw-dev}" \
	 PWRAP_JWT_SECRET="$${PWRAP_JWT_SECRET:-super-dev-jwt-secret-please-change-me-32bytes}" \
	 bash scripts/e2e.sh geo-spots

.PHONY: e2e-rls
e2e-rls: build ## run the Track A RLS demo (SDK + PostgREST isolation)
	@PWRAP_DATABASE_URL="$${PWRAP_DATABASE_URL:-postgres://pwrap:pwrap@localhost:5432/pwrap?sslmode=disable}" \
	 PWRAP_BOOTSTRAP_TOKEN="$${PWRAP_BOOTSTRAP_TOKEN:-dev-admin}" \
	 PWRAP_AUTHENTICATOR_PASSWORD="$${PWRAP_AUTHENTICATOR_PASSWORD:-authpw-dev}" \
	 PWRAP_JWT_SECRET="$${PWRAP_JWT_SECRET:-super-dev-jwt-secret-please-change-me-32bytes}" \
	 bash scripts/e2e.sh rls-notes

.PHONY: test
test: ## run all unit tests (fast, no docker)
	go test ./... -race -count=1

.PHONY: test-short
test-short: ## run tests, skip integration
	go test ./... -short -race

.PHONY: image
image: ## build pwrap-postgres:local (pgvector + pg_graphql + postgis)
	docker-compose build postgres

.PHONY: test-integration
test-integration: image ## run testcontainers integration suite (slow, needs docker)
	go test -tags integration ./internal/integration/... -count=1 -v

.PHONY: lint
lint: ## run golangci-lint
	golangci-lint run ./...

.PHONY: fmt
fmt: ## go fmt
	go fmt ./...

.PHONY: run
run: ## run pwrapd locally (requires postgres via `make pg`)
	PWRAP_DATABASE_URL="$${PWRAP_DATABASE_URL:-postgres://pwrap:pwrap@localhost:5432/pwrap?sslmode=disable}" \
		go run ./cmd/pwrapd

.PHONY: pg
pg: ## start local postgres via docker-compose
	docker-compose up -d postgres

.PHONY: pg-down
pg-down: ## stop local postgres
	docker-compose down

.PHONY: clean
clean: ## remove build artifacts
	rm -rf $(BIN)
