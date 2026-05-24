# Use bash so we can rely on pipefail in recipes.
SHELL := /bin/bash

# Arguments
V ?= 0
ifeq ($(V), 1)
override VERBOSE_TAG := -v
endif

# Variables
TEST_TIMEOUT   := 5m
# STRESS_TIMEOUT is the per-package timeout for `make stress-test`. Bumped from
# 30m → 45m on 2026-05-24 after the P0.2 byte-level chaos suite landed: the
# hsmsss_integration package's count=50 race runtime grew from ~27m to ~34m
# (broad + new P0.2 scenarios), exceeding the prior 30m budget by ~3-4m. The
# 45m budget restores ~10m headroom and aligns with the secs1 package's
# observed ~23m runtime under the same conditions. See stress-test-fuzz-flake
# memory for the unrelated FuzzConnectionLifecycle hang that requires
# -skip '^Fuzz'.
STRESS_TIMEOUT := 45m
STRESS_COUNT   ?= 50
FUZZ_TIME      ?= 30s
GO_TEST_P      ?= $(shell nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 8)

ALL_SRC        := $(shell find . -name "*.go")
ALL_SRC        += go.mod
TEST_DIRS      := $(sort $(dir $(filter %_test.go,$(ALL_SRC))))
LATEST_GIT_TAG := $(shell git describe --tags --abbrev=0 2>/dev/null)

# Packages with timing-sensitive tests exercised by stress-test.
# Add new packages here when they start producing flakes under contention.
STRESS_DIRS := ./hsmsss/... ./hsms/... ./secs1/... ./tests/...

# Packages that contain Fuzz* targets. fuzz-test auto-discovers the targets
# inside each package, so new fuzzers are picked up automatically.
FUZZ_PKGS := ./hsms ./hsmsss

# Coverage outputs.
COVER_ROOT            := ./.coverage
COVER_PROFILE         := $(COVER_ROOT)/coverprofile.out
SUMMARY_COVER_PROFILE := $(COVER_ROOT)/summary.out

# Linter tools are pinned in .linter.go.mod. $(LINTER_STAMP) gates downloads
# so `make lint` does not re-run `go mod download` on every invocation.
LINTER_MOD   := .linter.go.mod
LINTER_SUM   := .linter.go.sum
LINTER_STAMP := .tools-stamp
GOLANGCI     := go tool -modfile=$(LINTER_MOD) golangci-lint

.DEFAULT_GOAL := help

##@ Help

help: ## Print this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m [VAR=value ...]\n"} \
		/^[a-zA-Z0-9_.-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Tools

update-tools: ## Install/refresh linter toolchain (run once per clone; rerun if .linter.go.mod changes)
	@printf "Install/update linter tool...\n"
	@go mod download -modfile=$(LINTER_MOD)
	@touch $(LINTER_STAMP)

$(LINTER_STAMP): $(LINTER_MOD) $(LINTER_SUM)
	@$(MAKE) --no-print-directory update-tools

##@ Lint

lint: $(LINTER_STAMP) ## Run golangci-lint (version pinned via .linter.go.mod)
	@printf "Run linter...\n"
	@$(GOLANGCI) run

fmt: $(LINTER_STAMP) ## Apply golangci-lint fmt (goimports etc. per .golangci.yaml)
	@printf "Run formatter...\n"
	@$(GOLANGCI) fmt

vet: ## Run go vet across all packages
	@printf "Run go vet...\n"
	@go vet ./...

check: lint vet ## Run lint + vet (no file modifications)

##@ Tests

clean: ## Remove test.log and clear test cache
	@rm -f test.log
	@go clean -testcache

clean-coverage: ## Remove generated coverage artifacts
	@rm -rf $(COVER_ROOT)

build-tests: ## Compile tests without running them (-exec=true short-circuits execution)
	@printf "Build tests...\n"
	@go test -exec="true" -count=0 $(TEST_DIRS)

test: clean ## Run tests with -short, -race; streams output to stdout and test.log
	@printf "Run tests with V=$(V), timeout=$(TEST_TIMEOUT), parallelism=$(GO_TEST_P)...\n"
	@set -o pipefail; CGO_ENABLED=1 go test ./... -short -timeout=$(TEST_TIMEOUT) $(VERBOSE_TAG) -race -p $(GO_TEST_P) 2>&1 | tee test.log

test-all: clean ## Run full test suite (no -short; enables integration-style tests)
	@printf "Run full tests with V=$(V), timeout=$(TEST_TIMEOUT), parallelism=$(GO_TEST_P)...\n"
	@set -o pipefail; CGO_ENABLED=1 go test ./... -timeout=$(TEST_TIMEOUT) $(VERBOSE_TAG) -race -p $(GO_TEST_P) 2>&1 | tee test.log

bench: ## Run benchmarks across all packages (-benchmem, no unit tests)
	@printf "Run benchmarks...\n"
	@go test -run=^$$ -bench=. -benchmem ./...

# Stress tests: run tests many times under different scheduler conditions to
# surface timing-sensitive flakes.  Override STRESS_COUNT (default 50) to tune.
# Fuzz targets are excluded from stress because the hsmsss FuzzConnectionLifecycle
# target hangs reliably under -count=50 due to a Go cgo DNS-resolver
# singleflight pile-up (net.Listen on 127.0.0.1:0 goes through the resolver
# under load). Fuzz coverage is provided by `make fuzz-test` instead, which
# runs each Fuzz target once for a bounded time and does not exhibit the flake.
stress-test: clean ## Stress STRESS_DIRS under GOMAXPROCS=1 and default scheduler
	@printf "=== Stress test: GOMAXPROCS=1, count=$(STRESS_COUNT) (maximises goroutine contention) ===\n"
	@set -e; for d in $(STRESS_DIRS); do \
		GOMAXPROCS=1 CGO_ENABLED=1 go test $$d -count=$(STRESS_COUNT) -race -timeout=$(STRESS_TIMEOUT) -p 1 -skip '^Fuzz' $(VERBOSE_TAG); \
	done
	@printf "=== Stress test: default GOMAXPROCS, count=$(STRESS_COUNT), parallel=$(GO_TEST_P) ===\n"
	@set -e; for d in $(STRESS_DIRS); do \
		CGO_ENABLED=1 go test $$d -count=$(STRESS_COUNT) -race -timeout=$(STRESS_TIMEOUT) -p $(GO_TEST_P) -skip '^Fuzz' $(VERBOSE_TAG); \
	done
	@printf "=== All stress tests passed ($(STRESS_COUNT) iterations × 2 GOMAXPROCS modes) ===\n"

# Quick stress: runs only the most timing-sensitive tests for fast iteration.
stress-quick: clean ## Narrow stress run: only the known flake-prone tests
	@printf "=== Quick stress: flake-prone tests, count=$(STRESS_COUNT) ===\n"
	@GOMAXPROCS=1 CGO_ENABLED=1 go test ./hsmsss/... -run "TestConnection_Linktest|TestDrainMessage|TestSendRequestDrain|TestLinktestFail" \
		-count=$(STRESS_COUNT) -race -timeout=$(STRESS_TIMEOUT) -p 1 $(VERBOSE_TAG)
	@CGO_ENABLED=1 go test ./tests/hsmsss_integration/... -run "TestConcurrentClose|TestActiveExponentialBackoff" \
		-count=$(STRESS_COUNT) -race -timeout=$(STRESS_TIMEOUT) -p 1 $(VERBOSE_TAG)
	@printf "=== Quick stress passed ===\n"

# Fuzz tests: auto-discover every Fuzz* under FUZZ_PKGS and run each for FUZZ_TIME.
# Override FUZZ_TIME to tune, e.g.  make fuzz-test FUZZ_TIME=5m
fuzz-test: ## Run every Fuzz* target for FUZZ_TIME (default 30s)
	@printf "%s\n" "=== Fuzz tests (each target for $(FUZZ_TIME)) ==="
	@set -e; for pkg in $(FUZZ_PKGS); do \
		for name in $$(go test -list '^Fuzz' $$pkg 2>/dev/null | grep -E '^Fuzz' | sort -u); do \
			printf "%s\n" "-- $$name ($$pkg) --"; \
			CGO_ENABLED=1 go test $$pkg -run=^$$ -fuzz=$$name -race -fuzztime=$(FUZZ_TIME); \
		done; \
	done
	@printf "%s\n" "=== All fuzz tests completed ==="

##@ Coverage

$(COVER_ROOT):
	@mkdir -p $(COVER_ROOT)

coverage: $(COVER_ROOT) ## Produce per-package coverage profiles under $(COVER_ROOT)
	@printf "Run unit tests with coverage...\n"
	@echo "mode: atomic" > $(COVER_PROFILE)
	@set -e; for d in $(patsubst ./%/,%,$(TEST_DIRS)); do \
		mkdir -p $(COVER_ROOT)/$$d; \
		go test ./$$d -timeout=$(TEST_TIMEOUT) -race -coverprofile=$(COVER_ROOT)/$$d/coverprofile.out $(VERBOSE_TAG); \
		grep -v -e "^mode: \w\+" $(COVER_ROOT)/$$d/coverprofile.out >> $(COVER_PROFILE) || true; \
	done

.PHONY: $(SUMMARY_COVER_PROFILE)
$(SUMMARY_COVER_PROFILE): $(COVER_ROOT)
	@printf "Combine coverage reports to $(SUMMARY_COVER_PROFILE)...\n"
	@rm -f $(SUMMARY_COVER_PROFILE)
	@echo "mode: atomic" > $(SUMMARY_COVER_PROFILE)
	@for f in $(wildcard $(COVER_ROOT)/*coverprofile.out); do \
		printf "Add %s...\n" $$f; \
		grep -v -e "[Mm]ocks\?.go" -e "^mode: \w\+" $$f >> $(SUMMARY_COVER_PROFILE) || true; \
	done

coverage-report: $(SUMMARY_COVER_PROFILE) ## Render HTML coverage report next to the summary profile
	@printf "Generate HTML report from $(SUMMARY_COVER_PROFILE) to $(SUMMARY_COVER_PROFILE).html...\n"
	@go tool cover -html=$(SUMMARY_COVER_PROFILE) -o $(SUMMARY_COVER_PROFILE).html

##@ Module

update-gomod: gomod-tidy gomod-vendor ## Tidy + vendor

gomod-tidy: ## go mod tidy
	@printf "go mod tidy...\n"
	@go mod tidy

gomod-vendor: ## go mod vendor
	@printf "go mod vendor...\n"
	@go mod vendor

mod-verify: ## Verify module checksums (go mod verify)
	@printf "go mod verify...\n"
	@go mod verify

update-pkg-cache: ## Prime pkg.go.dev with the latest git tag
	@printf "Update package cache with latest git tag: $(LATEST_GIT_TAG)\n"
	@curl -s https://proxy.golang.org/github.com/arloliu/go-secs/@v/$(LATEST_GIT_TAG).info > /dev/null

##@ Composite

ci: check test ## Single entry point for CI (lint + vet + -short tests)

.PHONY: help update-tools lint fmt vet check \
        clean clean-coverage build-tests test test-all bench \
        stress-test stress-quick fuzz-test \
        coverage coverage-report \
        update-gomod gomod-tidy gomod-vendor mod-verify update-pkg-cache \
        ci
