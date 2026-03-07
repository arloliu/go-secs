# Arguments
V ?= 0
ifeq ($(V), 1)
override VERBOSE_TAG := -v
endif

# Variables
GOBIN := $(if $(shell go env GOBIN),$(shell go env GOBIN),$(GOPATH)/bin)
PATH := $(GOBIN):$(PATH)

define NEWLINE


endef

TEST_TIMEOUT := 5m
STRESS_TIMEOUT := 30m
STRESS_COUNT ?= 50
FUZZ_TIME ?= 30s
GO_TEST_P ?= $(shell nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 8)

ALL_SRC         := $(shell find . -name "*.go")
ALL_SRC         += go.mod
TEST_DIRS       := $(sort $(dir $(filter %_test.go,$(ALL_SRC))))
LATEST_GIT_TAG  := $(shell git describe --tags --abbrev=0)

# Code coverage output files.
COVER_ROOT                 := ./.coverage
COVER_PROFILE         := $(COVER_ROOT)/coverprofile.out
SUMMARY_COVER_PROFILE      := $(COVER_ROOT)/summary.out

# Tools
update-tools:
	@printf "Install/update linter tool...\n"
	@go mod download -modfile=.linter.go.mod

# Tests
clean:
	@rm -f test.log
	@go clean -testcache

build-tests:
	@printf "Build tests...\n"
	@go test -exec="true" -count=0 $(TEST_DIRS)

test: clean
	@printf "Run tests with V=$(V), timeout=$(TEST_TIMEOUT), parallelism=$(GO_TEST_P)...\n"
	@CGO_ENABLED=1 go test ./... -short -timeout=$(TEST_TIMEOUT) $(VERBOSE_TAG) -race -p $(GO_TEST_P) > test.log 2>&1 || { cat test.log; exit 1; }
	@cat test.log

# Update https://pkg.go.dev/
update-pkg-cache:
	@printf "Update package cache with latest git tag: $(LATEST_GIT_TAG)\n"
	@curl -s https://proxy.golang.org/github.com/arloliu/go-secs/@v/$(LATEST_GIT_TAG).info > /dev/null

##### Coverage #####
$(COVER_ROOT):
	@mkdir -p $(COVER_ROOT)

coverage: $(COVER_ROOT)
	@printf "Run unit tests with coverage...\n"
	@echo "mode: atomic" > $(COVER_PROFILE)
	$(foreach TEST_DIR,$(patsubst ./%/,%,$(TEST_DIRS)),\
		@mkdir -p $(COVER_ROOT)/$(TEST_DIR); \
		go test ./$(TEST_DIR) -timeout=$(TEST_TIMEOUT) -race -coverprofile=$(COVER_ROOT)/$(TEST_DIR)/coverprofile.out || exit 1; \
		grep -v -e "^mode: \w\+" $(COVER_ROOT)/$(TEST_DIR)/coverprofile.out >> $(COVER_PROFILE) || true \
	$(NEWLINE))

.PHONY: $(SUMMARY_COVER_PROFILE)
$(SUMMARY_COVER_PROFILE): $(COVER_ROOT)
	@printf "Combine coverage reports to $(SUMMARY_COVER_PROFILE)...\n"
	@rm -f $(SUMMARY_COVER_PROFILE)
	@echo "mode: atomic" > $(SUMMARY_COVER_PROFILE)
	$(foreach COVER_PROFILE,$(wildcard $(COVER_ROOT)/*coverprofile.out),\
		@printf "Add %s...\n" $(COVER_PROFILE); \
		grep -v -e "[Mm]ocks\?.go" -e "^mode: \w\+" $(COVER_PROFILE) >> $(SUMMARY_COVER_PROFILE) || true \
	$(NEWLINE))

coverage-report: $(SUMMARY_COVER_PROFILE)
	@printf "Generate HTML report from $(SUMMARY_COVER_PROFILE) to $(SUMMARY_COVER_PROFILE).html...\n"
	@go tool cover -html=$(SUMMARY_COVER_PROFILE) -o $(SUMMARY_COVER_PROFILE).html

# Checks
check: lint vet

lint: update-tools
	@printf "Run linter...\n"
	@go tool -modfile=.linter.go.mod golangci-lint run

# Stress tests: run tests many times under different scheduler conditions to
# surface timing-sensitive flakes.  Override STRESS_COUNT (default 50) to tune.
stress-test: clean
	@printf "=== Stress test: GOMAXPROCS=1, count=$(STRESS_COUNT) (maximises goroutine contention) ===\n"
	@GOMAXPROCS=1 CGO_ENABLED=1 go test ./hsmsss/... -count=$(STRESS_COUNT) -race -timeout=$(STRESS_TIMEOUT) -p 1 || exit 1
	@GOMAXPROCS=1 CGO_ENABLED=1 go test ./hsms/... -count=$(STRESS_COUNT) -race -timeout=$(STRESS_TIMEOUT) -p 1 || exit 1
	@GOMAXPROCS=1 CGO_ENABLED=1 go test ./secs1/... -count=$(STRESS_COUNT) -race -timeout=$(STRESS_TIMEOUT) -p 1 || exit 1
	@GOMAXPROCS=1 CGO_ENABLED=1 go test ./tests/... -count=$(STRESS_COUNT) -race -timeout=$(STRESS_TIMEOUT) -p 1 || exit 1
	@printf "=== Stress test: default GOMAXPROCS, count=$(STRESS_COUNT), parallel=$(GO_TEST_P) ===\n"
	@CGO_ENABLED=1 go test ./hsmsss/... -count=$(STRESS_COUNT) -race -timeout=$(STRESS_TIMEOUT) -p $(GO_TEST_P) || exit 1
	@CGO_ENABLED=1 go test ./hsms/... -count=$(STRESS_COUNT) -race -timeout=$(STRESS_TIMEOUT) -p $(GO_TEST_P) || exit 1
	@CGO_ENABLED=1 go test ./secs1/... -count=$(STRESS_COUNT) -race -timeout=$(STRESS_TIMEOUT) -p $(GO_TEST_P) || exit 1
	@CGO_ENABLED=1 go test ./tests/... -count=$(STRESS_COUNT) -race -timeout=$(STRESS_TIMEOUT) -p $(GO_TEST_P) || exit 1
	@printf "=== All stress tests passed ($(STRESS_COUNT) iterations × 2 GOMAXPROCS modes) ===\n"

# Quick stress: runs only the most timing-sensitive tests for fast iteration.
stress-quick: clean
	@printf "=== Quick stress: flake-prone tests, count=$(STRESS_COUNT) ===\n"
	@GOMAXPROCS=1 CGO_ENABLED=1 go test ./hsmsss/... -run "TestConnection_Linktest|TestDrainMessage|TestSendRequestDrain|TestLinktestFail" \
		-count=$(STRESS_COUNT) -race -timeout=$(STRESS_TIMEOUT) -p 1 || exit 1
	@CGO_ENABLED=1 go test ./tests/hsmsss_integration/... -run "TestConcurrentClose|TestActiveExponentialBackoff" \
		-count=$(STRESS_COUNT) -race -timeout=$(STRESS_TIMEOUT) -p 1 || exit 1
	@printf "=== Quick stress passed ===\n"

# Fuzz tests: run all fuzz targets for FUZZ_TIME each (default 30s).
# Override FUZZ_TIME to tune, e.g.  make fuzz-test FUZZ_TIME=5m
fuzz-test:
	@printf "%s\n" "=== Fuzz tests (each target for $(FUZZ_TIME)) ==="
	@printf "%s\n" "-- FuzzDecodeMessage (hsms) --"
	@CGO_ENABLED=1 go test ./hsms/... -run=^$$ -fuzz=FuzzDecodeMessage -race -fuzztime=$(FUZZ_TIME)
	@printf "%s\n" "-- FuzzDecodeHSMSMessage (hsms) --"
	@CGO_ENABLED=1 go test ./hsms/... -run=^$$ -fuzz=FuzzDecodeHSMSMessage -race -fuzztime=$(FUZZ_TIME)
	@printf "%s\n" "-- FuzzMessageReader_ReadMessage (hsmsss) --"
	@CGO_ENABLED=1 go test ./hsmsss/... -run=^$$ -fuzz=FuzzMessageReader_ReadMessage -race -fuzztime=$(FUZZ_TIME)
	@printf "%s\n" "-- FuzzConnectionLifecycle (hsmsss) --"
	@CGO_ENABLED=1 go test ./hsmsss/... -run=^$$ -fuzz=FuzzConnectionLifecycle -race -fuzztime=$(FUZZ_TIME)
	@printf "%s\n" "=== All fuzz tests completed ==="

# Misc
update-gomod: gomod-tidy gomod-vendor

gomod-tidy:
	@printf "go mod tidy...\n"
	@go mod tidy

gomod-vendor:
	@printf "go mod vendor...\n"
	@go mod vendor
