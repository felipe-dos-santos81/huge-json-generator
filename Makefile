# Makefile for jsongen — Huge Random JSON Payload Generator
# Targets: help → build → generate/bench → test/check → clean
SERVICE = jsongen

# Variables
BIN = bin/jsongen
OUT_DIR = out
size ?= 1GB
schema ?= chaos
EXT = $(if $(ndjson),ndjson,json)

.PHONY: help build clean generate bench test test-race vet fmt check

# ── Environment ──────────────────────────────────────────────────────────────

help: ## Print this help message
	@printf '\033[01;32m${SERVICE} — Huge Random JSON Payload Generator\033[00;37m\n\n'
	@printf "\033[33mUsage:\033[0m\n  make [target] [arg=\"val\"...]\n\n\033[33mTargets:\033[0m\n"
	@grep -E '^[-a-zA-Z0-9_\.\/]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; \
		{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build: ## Build the binary into bin/jsongen
	go build -o $(BIN) .

clean: ## Remove bin/ and out/ (generated payloads)
	rm -rf bin $(OUT_DIR)
	@echo "Cleanup complete."

# ── Generate ─────────────────────────────────────────────────────────────────

generate: build ## Write a payload to out/ (usage: make generate size=50GB [schema=typed] [ndjson=1] [args="-seed 7 -depth 8"])
	@mkdir -p $(OUT_DIR)
	@echo "Generating $(size) of $(schema) JSON into $(OUT_DIR)/$(schema)-$(size).$(EXT)..."
	$(BIN) -size "$(size)" -schema "$(schema)" $(if $(ndjson),-ndjson) $(args) > $(OUT_DIR)/$(schema)-$(size).$(EXT)

bench: build ## Measure throughput to /dev/null for both schemas (usage: make bench [size=1GB] [args="-workers 1"])
	@for s in chaos typed; do \
		printf '%-6s ' $$s; \
		$(BIN) -size "$(size)" -schema $$s $(args) 2>&1 >/dev/null | tr '\r' '\n' | grep '^done'; \
	done

# ── Development ───────────────────────────────────────────────────────────────

test: ## Run the unit test suite
	go test -count=1 ./...

test-race: ## Run the unit test suite under the race detector (~15 s)
	go test -count=1 -race ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format all Go files in place
	gofmt -w .

check: ## Fail on unformatted code, then vet and test (run before committing)
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run: make fmt"; exit 1; }
	go vet ./...
	go test -count=1 ./...
