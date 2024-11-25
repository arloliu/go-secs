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
	@go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.62.0

# Tests
clean:
	@rm -f test.log
	@go clean -testcache

build-tests:
	@printf "Build tests...\n"
	@go test -exec="true" -count=0 $(TEST_DIRS)

test: clean
	@printf "Run tests...\n"
	$(foreach TEST_DIR,$(TEST_DIRS),\
		@CGO_ENABLED=1  go test $(TEST_DIR) -short -timeout=$(TEST_TIMEOUT) $(VERBOSE_TAG) -race | tee -a test.log \
	$(NEWLINE))
	@! grep -q "^--- FAIL" test.log

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
	@golangci-lint run

# Misc
update-gomod: gomod-tidy gomod-vendor

gomod-tidy:
	@printf "go mod tidy...\n"
	@go mod tidy

gomod-vendor:
	@printf "go mod vendor...\n"
	@go mod vendor
