# Makefile for jsongen — Huge Random JSON Generator
# Generate targets write to out/ and accept size= (default 1GB) and args="extra flags".
SERVICE = jsongen

# Variables
BIN     = bin/jsongen
OUT_DIR = out
size   ?= 1GB
schema ?= chaos
EXT     = $(if $(ndjson),ndjson,json)

.PHONY: help build clean generate shape corpus bench test test-race vet fmt check

# ── Build ────────────────────────────────────────────────────────────────────

help: ## List targets
	@printf '\033[01;32m${SERVICE} — Huge Random JSON Generator\033[00;37m\n\n'
	@printf "\033[33mUsage:\033[0m\n  make [target] [arg=\"val\"...]\n\n\033[33mTargets:\033[0m\n"
	@grep -E '^[-a-zA-Z0-9_\.\/]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; \
		{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ## Build bin/jsongen
	go build -o $(BIN) .

clean: ## Remove bin/ and out/
	rm -rf bin $(OUT_DIR)

# ── Generate ─────────────────────────────────────────────────────────────────

generate: build ## Records: make generate [schema=typed] [ndjson=1]
	@mkdir -p $(OUT_DIR)
	$(BIN) -size "$(size)" -schema "$(schema)" $(if $(ndjson),-ndjson) $(args) > $(OUT_DIR)/$(schema)-$(size).$(EXT)

shape: build ## Extreme values: make shape shape=deep [cap=10MB] [ndjson=1]
	@mkdir -p $(OUT_DIR)
	$(BIN) -shape "$(shape)" -size "$(size)" $(if $(cap),-cap "$(cap)") $(if $(ndjson),-ndjson) $(args) > $(OUT_DIR)/shape-$(shape)-$(size).$(EXT)

corpus: build ## Labelled parser test cases: make corpus
	@mkdir -p $(OUT_DIR)
	$(BIN) -corpus -size "$(size)" $(args) > $(OUT_DIR)/corpus-$(size).ndjson

bench: build ## Throughput of both schemas, written to /dev/null
	@for s in chaos typed; do \
		printf '%-6s ' $$s; \
		$(BIN) -size "$(size)" -schema $$s $(args) 2>&1 >/dev/null | grep '^done'; \
	done

# ── Develop ──────────────────────────────────────────────────────────────────

check: ## gofmt check, vet and tests (run before committing)
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run: make fmt"; exit 1; }
	go vet ./...
	go test -count=1 ./...

test: ## Tests
	go test -count=1 ./...

test-race: ## Tests under the race detector (slow)
	go test -count=1 -race ./...

vet: ## go vet
	go vet ./...

fmt: ## Format Go files in place
	gofmt -w .
