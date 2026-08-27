GO      ?= go
LINT    ?= $(shell command -v golangci-lint 2>/dev/null || echo $(HOME)/go/bin/golangci-lint)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build test test-unit test-e2e lint fmt e2e-up e2e-down clean

all: lint test build

build:
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o runnerforge ./cmd/runnerforge

## test-unit runs everything that needs no external services.
test-unit:
	$(GO) test -race -count=1 ./...

## test runs the full suite. End-to-end tests skip themselves unless the
## matching RF_TEST_* variables are set; see e2e-up.
test: test-unit

## e2e-up starts the local forges the end-to-end tests run against.
## Use: eval "$$(make -s e2e-up)"     (add RF_WITH_GITLAB=1 for GitLab)
e2e-up:
	@testdata/e2e-up.sh

e2e-down:
	-docker rm -f rf-forgejo rf-gitlab 2>/dev/null
	-docker network rm rf-net 2>/dev/null

lint:
	$(LINT) run ./...

fmt:
	$(GO) fmt ./...
	$(LINT) fmt ./... 2>/dev/null || true

clean:
	rm -f runnerforge *.db *.db-*
