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
# 30m → 45m on 2026-05-24 after the P0.2 byte-level chaos suite landed (see git
# history for that round). On 2026-07-11, hsmsss's count=50 -race GOMAXPROCS=1
# runtime had grown to ~51.2m (byte-level fault-injection proxy and
# leak-detection harness additions), exceeding the 45m budget — confirmed via
# a standalone `go test ./hsmsss/... -count=50 -race -timeout=90m -p 1` run
# that passed clean (6750/6750 subtests, 0 failures) at 3072s. Rather than
# raise the timeout to match a 50x stress multiplier, STRESS_COUNT was lowered
# 50 → 10 (dominated by one deliberately-serial, TCP-heavy test — see
# TestHSMS_StrandedSend_NoFrameAcrossGeneration — that alone took ~25m of the
# 51.2m at count=50; 10 repetitions still exercises every race window
# meaningfully without the multiplicative blowup). At count=10, hsmsss's
# measured per-iteration cost (61.44s, from the 3072s/50 count=50 run) implies
# ~10.24m; STRESS_TIMEOUT was set to 15m, ~47% headroom, matching the prior
# budget's headroom ratio. Fuzz seed corpora are included in stress (each
# iteration is watchdog-bounded — see the stress-test target comment); they
# add ~0.5s/count and do not threaten this budget.
# On 2026-08-14 the generation-isolation hardening round (stale-generation
# barrier/binding tests, refused-TCP-up tests, port-reservation flocks, wire
# silence windows) grew hsmsss's count=10 GOMAXPROCS=1 runtime to 899.959s
# (~15.0m, measured standalone with -timeout=30m, passing clean) — exactly at
# the 15m budget, which a full-run build contention then tipped over. Same
# ~47% headroom ratio on the new measurement gives 22m.
STRESS_TIMEOUT := 22m
STRESS_COUNT   ?= 10
FUZZ_TIME      ?= 30s
GO_TEST_P      ?= $(shell nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 8)

# go list stays within the root module and skips hidden directories.
# GOWORK=off keeps these variables on the root module when a developer has a
# local go.work for the nested tracepack module: `go list -m` would otherwise
# print every workspace module.
TEST_DIRS      := $(sort $(patsubst $(CURDIR),./,$(patsubst $(CURDIR)/%,./%/,$(shell GOWORK=off go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.Dir}}{{end}}' ./...))))
# Only root-module tags (vX.Y.Z); `git describe --tags` alone would return a
# newer tracepack/vX.Y.Z tag.
LATEST_GIT_TAG := $(shell git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null)
MODULE_PATH    := $(shell GOWORK=off go list -m 2>/dev/null)

# Packages with timing-sensitive tests exercised by stress-test.
# Add new packages here when they start producing flakes under contention.
STRESS_DIRS := ./hsmsss/... ./hsms/... ./secs1/... ./integration/...

# Packages that contain Fuzz* targets. fuzz-test auto-discovers the targets
# inside each package, so new fuzzers in a listed package are picked up
# automatically; a package's first fuzzer needs an entry here.
FUZZ_PKGS := ./hsms ./hsmsss ./integration ./secs2 ./sml

# Coverage outputs.
COVER_ROOT            := ./.coverage
COVER_PROFILE         := $(COVER_ROOT)/coverprofile.out
SUMMARY_COVER_PROFILE := $(COVER_ROOT)/summary.out

# golangci-lint is pinned in mise.toml; `mise install` puts that version on PATH.
GOLANGCI ?= golangci-lint

.DEFAULT_GOAL := help

##@ Help

help: ## Print this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m [VAR=value ...]\n"} \
		/^[a-zA-Z0-9_.-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Lint

lint: ## Run golangci-lint (version pinned in mise.toml)
	@printf "Run linter...\n"
	@$(GOLANGCI) run

fmt: ## Apply golangci-lint fmt (goimports etc. per .golangci.yaml)
	@printf "Run formatter...\n"
	@$(GOLANGCI) fmt

vet: ## Run go vet across all packages
	@printf "Run go vet...\n"
	@go vet ./...

check: lint vet ## Run lint + vet (no file modifications)

docs-check: ## Check docs/ against .agents/rules/450-doc-lifecycle.md (Status lines, subject READMEs, relative links)
	@printf "Run docs check...\n"
	@python3 scripts/docs-check.py

##@ Generator (tools/gemgen)

# tools/gemgen is its own Go module (see docs/specs/2026-07-07-gem-codegen-design.md)
# so root `lint`/`test`/`ci` never reach it -- `go test ./...`/`golangci-lint run`
# only traverse the current module. These targets are the only way to exercise it.
GEMGEN_DIR := tools/gemgen

lint-gemgen: ## Run the pinned linter against tools/gemgen (default + integration build tag)
	@printf "Run gemgen linter...\n"
	@cd $(GEMGEN_DIR) && $(GOLANGCI) run --config ../../.golangci.yaml ./...
	@cd $(GEMGEN_DIR) && $(GOLANGCI) run --config ../../.golangci.yaml --build-tags integration ./...

test-gemgen: ## Run tools/gemgen's own unit tests (schema, load/validate, params, render)
	@printf "Run gemgen tests...\n"
	@cd $(GEMGEN_DIR) && go test ./... -race

test-gemgen-integration: ## Run gemgen's real-compile guard (shells out to `go build` against secs2; NOT covered by test-gemgen)
	@printf "Run gemgen integration tests...\n"
	@cd $(GEMGEN_DIR) && go test -tags integration ./... -race

##@ tracepack (nested module)

# tracepack/ is the nested module github.com/arloliu/go-secs/tracepack
# (spec in docs/specs/tracepack/, tags tracepack/vX.Y.Z). Root lint/test/ci
# never reach it. Local development uses a go.work (`make work`, gitignored);
# the *-consumer targets run with GOWORK=off so they see exactly what a
# consumer of the published module sees, and they must pass before any
# tracepack/ tag.
TRACEPACK_DIR         := tracepack
TRACEPACK_MODULE_PATH := $(shell cd $(TRACEPACK_DIR) && GOWORK=off go list -m 2>/dev/null)
TRACEPACK_LATEST_TAG  := $(shell git describe --tags --abbrev=0 --match 'tracepack/v[0-9]*' 2>/dev/null)

work: ## Create the local go.work for root + tracepack development (gitignored)
	@printf "go work init . ./$(TRACEPACK_DIR)...\n"
	@go work init . ./$(TRACEPACK_DIR)

lint-tracepack: ## Run the pinned linter against tracepack/ (root .golangci.yaml)
	@printf "Run tracepack linter...\n"
	@cd $(TRACEPACK_DIR) && $(GOLANGCI) run --config ../.golangci.yaml ./...

test-tracepack: ## Run tracepack's tests with -race
	@printf "Run tracepack tests...\n"
	@cd $(TRACEPACK_DIR) && CGO_ENABLED=1 go test ./... -timeout=$(TEST_TIMEOUT) $(VERBOSE_TAG) -race

test-tracepack-386: ## Vet and test tracepack where int is 32 bits (GOARCH=386, GOWORK=off; no -race on 386)
	@printf "Run tracepack tests for GOARCH=386...\n"
	@cd $(TRACEPACK_DIR) && GOWORK=off GOARCH=386 go vet ./...
	@cd $(TRACEPACK_DIR) && GOWORK=off GOARCH=386 go test ./... -timeout=$(TEST_TIMEOUT) $(VERBOSE_TAG)

check-tracepack-consumer: ## Consumer view of tracepack/: GOWORK=off, tidy go.mod, build, -race tests (pre-tag gate)
	@printf "Check tracepack as a consumer (GOWORK=off)...\n"
	@cd $(TRACEPACK_DIR) && GOWORK=off go mod tidy -diff
	@cd $(TRACEPACK_DIR) && GOWORK=off go build ./...
	@cd $(TRACEPACK_DIR) && GOWORK=off CGO_ENABLED=1 go test ./... -timeout=$(TEST_TIMEOUT) $(VERBOSE_TAG) -race

fuzz-tracepack: ## Run every Fuzz* target under tracepack/ for FUZZ_TIME (default 30s)
	@printf "%s\n" "=== tracepack fuzz tests (each target for $(FUZZ_TIME)) ==="
	@set -e; cd $(TRACEPACK_DIR); for pkg in $$(go list ./...); do \
		for name in $$(go test -list '^Fuzz' $$pkg 2>/dev/null | grep -E '^Fuzz' | sort -u); do \
			printf "%s\n" "-- $$name ($$pkg) --"; \
			CGO_ENABLED=1 go test $$pkg -run=^$$ -fuzz=$$name -race -fuzztime=$(FUZZ_TIME); \
		done; \
	done
	@printf "%s\n" "=== All tracepack fuzz tests completed ==="

update-pkg-cache-tracepack: ## Prime the Go module proxy with the latest tracepack/vX.Y.Z tag
	@printf "Priming module proxy cache for $(TRACEPACK_MODULE_PATH)@$(TRACEPACK_LATEST_TAG:tracepack/%=%)...\n"
	@curl -s https://proxy.golang.org/$(TRACEPACK_MODULE_PATH)/@v/$(TRACEPACK_LATEST_TAG:tracepack/%=%).info > /dev/null

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

test-all: clean test-gemgen test-gemgen-integration ## Run full test suite (no -short; enables integration-style tests in the root module) plus the gemgen module's own tests and its build-tagged compile guard
	@printf "Run full tests with V=$(V), timeout=$(TEST_TIMEOUT), parallelism=$(GO_TEST_P)...\n"
	@set -o pipefail; CGO_ENABLED=1 go test ./... -timeout=$(TEST_TIMEOUT) $(VERBOSE_TAG) -race -p $(GO_TEST_P) 2>&1 | tee test.log

bench: ## Run benchmarks across all packages (-benchmem, no unit tests)
	@printf "Run benchmarks...\n"
	@go test -run=^$$ -bench=. -benchmem ./...

# Stress tests: run tests many times under different scheduler conditions to
# surface timing-sensitive flakes.  Override STRESS_COUNT (default 50) to tune.
# Fuzz targets ARE included: FuzzConnectionLifecycle's seed corpus re-exercises the
# Open/Close/Send/UpdateConfig lifecycle under -race, and each iteration is bounded by
# an in-harness 5s watchdog (hsmsss/fuzz_test.go), so it cannot hang the run. (An older
# getRandomListener-based harness could pile up on the cgo DNS resolver under -count;
# the current harness listens/dials on literal 127.0.0.1, which never hits the
# resolver.) Broader fuzz coverage still comes from `make fuzz-test`.
stress-test: clean ## Stress STRESS_DIRS under GOMAXPROCS=1 and default scheduler
	@printf "=== Stress test: GOMAXPROCS=1, count=$(STRESS_COUNT) (maximises goroutine contention) ===\n"
	@set -e; for d in $(STRESS_DIRS); do \
		GOMAXPROCS=1 CGO_ENABLED=1 go test $$d -count=$(STRESS_COUNT) -race -timeout=$(STRESS_TIMEOUT) -p 1 $(VERBOSE_TAG); \
	done
	@printf "=== Stress test: default GOMAXPROCS, count=$(STRESS_COUNT), parallel=$(GO_TEST_P) ===\n"
	@set -e; for d in $(STRESS_DIRS); do \
		CGO_ENABLED=1 go test $$d -count=$(STRESS_COUNT) -race -timeout=$(STRESS_TIMEOUT) -p $(GO_TEST_P) $(VERBOSE_TAG); \
	done
	@printf "=== All stress tests passed ($(STRESS_COUNT) iterations × 2 GOMAXPROCS modes) ===\n"

# Quick stress: runs only the most timing-sensitive tests for fast iteration.
stress-quick: clean ## Narrow stress run: only the known flake-prone tests
	@printf "=== Quick stress: flake-prone tests, count=$(STRESS_COUNT) ===\n"
	@GOMAXPROCS=1 CGO_ENABLED=1 go test ./hsmsss/... -run "TestLinktest_ThresholdDisconnect|TestLinktest_AutoFiresWhileSelected|TestHSMS_LinktestFailThreshold_ResetsOnSuccess|TestHSMS_StrandedSend_PostCloseGateAndReopenHealthCheck|TestChaos_DroppedLinktestRsp|TestChaos_RapidLinktestToggle" \
		-count=$(STRESS_COUNT) -race -timeout=$(STRESS_TIMEOUT) -p 1 $(VERBOSE_TAG)
	@CGO_ENABLED=1 go test ./hsmsss/... -run "TestConcurrentClose|TestHSMS_CloseRace_BoundedCleanShutdown|TestHSMS_SelectCloseRace_NoPanicNoZombie|TestActiveReconnectCadence_ExponentialBackoff|TestSocketRace_" \
		-count=$(STRESS_COUNT) -race -timeout=$(STRESS_TIMEOUT) -p 1 $(VERBOSE_TAG)
	@CGO_ENABLED=1 go test ./hsms/... -run "TestSocketRace_" \
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

##@ Release

update-pkg-cache: ## Prime the Go module proxy (and transitively pkg.go.dev) with the latest git tag
	@printf "Priming module proxy cache for $(MODULE_PATH)@$(LATEST_GIT_TAG)...\n"
	@curl -s https://proxy.golang.org/$(MODULE_PATH)/@v/$(LATEST_GIT_TAG).info > /dev/null

##@ Composite

ci: check docs-check test test-gemgen test-gemgen-integration lint-gemgen lint-tracepack test-tracepack ## Single entry point for CI (lint + vet + docs check + -short tests + gemgen and tracepack module gates)

.PHONY: help lint fmt vet check docs-check \
        lint-gemgen test-gemgen test-gemgen-integration \
        work lint-tracepack test-tracepack test-tracepack-386 check-tracepack-consumer fuzz-tracepack update-pkg-cache-tracepack \
        clean clean-coverage build-tests test test-all bench \
        stress-test stress-quick fuzz-test \
        coverage coverage-report \
        update-gomod gomod-tidy gomod-vendor mod-verify update-pkg-cache \
        ci
