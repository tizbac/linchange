# linchange developer tooling.
#
# `make help` lists the targets. The default target builds the server and the
# web interface.

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

BINARY      := bin/linchange
PKG         := ./...
GO          ?= go
NPM         ?= npm
WEB_DIR     := web
CERT_DIR    := .certs
DEV_CONFIG  := linchange.dev.yaml
COMPOSE     ?= docker compose

# Test containers. Override to point at an instance you already have.
PG_HOST     ?= 127.0.0.1
PG_PORT     ?= 55432
PG_USER     ?= linchange
PG_PASS     ?= linchange
PG_DB       ?= linchange
TEST_DSN    := postgres://$(PG_USER):$(PG_PASS)@$(PG_HOST):$(PG_PORT)/$(PG_DB)?sslmode=disable

##@ General

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Build

.PHONY: build
build: build-server ## Build the server binary

.PHONY: build-server
build-server: ## Build the server binary into bin/
	@mkdir -p bin
	$(GO) build -trimpath -o $(BINARY) ./cmd/linchange
	@echo "built $(BINARY)"

.PHONY: web
web: ## Install web interface dependencies
	cd $(WEB_DIR) && $(NPM) ci

.PHONY: web-build
web-build: ## Build the web interface into internal/httpapi/web/dist
	cd $(WEB_DIR) && $(NPM) run build

.PHONY: all
all: build-server web-build ## Build the server and the web interface

##@ Quality

.PHONY: fmt
fmt: ## Format Go code
	$(GO) fmt $(PKG)

.PHONY: vet
vet: ## Run go vet
	$(GO) vet $(PKG)

.PHONY: lint
lint: vet ## Run the full Go quality gate
	@command -v staticcheck >/dev/null 2>&1 && staticcheck $(PKG) || \
		echo "staticcheck not installed; skipping (go install honnef.co/go/tools/cmd/staticcheck@latest)"

.PHONY: test
test: test-unit test-integration ## Run every test

.PHONY: test-unit
test-unit: ## Run tests that do not need PostgreSQL
	$(GO) test -race -count=1 $(PKG)

.PHONY: test-integration
test-integration: ## Run tests, including those that need PostgreSQL
	@set -o pipefail; \
	LINCHANGE_TEST_DATABASE_URL="$(TEST_DSN)" $(GO) test -race -count=1 -v $(PKG) \
		2>&1 | tee .test-integration.log | grep -E '^(ok|FAIL|---)'; \
	status=$${PIPESTATUS[0]}; \
	if grep -q -- '--- SKIP' .test-integration.log; then \
		echo "error: tests skipped; integration coverage did not run"; \
		grep -- '--- SKIP' .test-integration.log; \
		status=1; \
	fi; \
	rm -f .test-integration.log; \
	exit $$status

.PHONY: check-config
check-config: build-server ## Validate a configuration file: make check-config CONFIG=path.yaml
	@./$(BINARY) --config "$(CONFIG)" --check-config

.PHONY: tidy
tidy: ## Tidy and verify go.mod
	$(GO) mod tidy
	$(GO) mod verify

##@ Local development

.PHONY: dev-certs
dev-certs: ## Generate a self-signed certificate for local development
	@mkdir -p $(CERT_DIR)
	@openssl req -x509 -newkey rsa:2048 -sha256 -days 365 -nodes \
		-keyout $(CERT_DIR)/key.pem -out $(CERT_DIR)/cert.pem \
		-subj "/CN=localhost" \
		-addext "subjectAltName=DNS:localhost,DNS:mail.localhost,IP:127.0.0.1" \
		2>/dev/null
	@echo "wrote $(CERT_DIR)/cert.pem and $(CERT_DIR)/key.pem"

.PHONY: up
up: ## Start PostgreSQL for local development
	$(COMPOSE) up -d postgres
	@echo "waiting for PostgreSQL..."
	@until $(COMPOSE) exec -T postgres pg_isready -U $(PG_USER) >/dev/null 2>&1; do sleep 1; done
	@echo "PostgreSQL is ready on $(PG_HOST):$(PG_PORT)"

.PHONY: down
down: ## Stop local development services
	$(COMPOSE) down -v

.PHONY: dev-config
dev-config: dev-certs ## Write a development configuration file
	@printf 'JWT_SECRET=%s\nAUTH_ENCRYPTION_KEY=%s\nDKIM_ENCRYPTION_KEY=%s\n' \
		"$$(head -c 32 /dev/urandom | base64 | tr -d '\n=')" \
		"$$(head -c 32 /dev/urandom | base64 | tr -d '\n=')" \
		"$$(head -c 32 /dev/urandom | base64 | tr -d '\n=')" > .env
	@sed -e 's|^  url: .*|  url: $(TEST_DSN)|' \
	     -e 's|^  cert_file: .*|  cert_file: $(CURDIR)/$(CERT_DIR)/cert.pem|' \
	     -e 's|^  key_file: .*|  key_file: $(CURDIR)/$(CERT_DIR)/key.pem|' \
	     -e 's|^  data_dir: .*|  data_dir: ./data|' \
	     -e 's|^  # imaps: .*|  imaps: { addr: ":993" }|' \
	     -e 's|^  # pop3s: .*|  pop3s: { addr: ":995" }|' \
	     -e 's|^  # smtp_submission_tls: .*|  smtp_submission_tls: { addr: ":465" }|' \
	     config/linchange.example.yaml > $(DEV_CONFIG)
	@echo "wrote $(DEV_CONFIG) and .env (keep .env out of version control)"

.PHONY: dev
dev: dev-config build-server ## Build and run the server against local development services
	@set -a; . ./.env; set +a; ./$(BINARY) --config $(DEV_CONFIG)

.PHONY: clean
clean: ## Remove build output
	rm -rf bin data .certs
