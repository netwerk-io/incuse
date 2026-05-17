.PHONY: all build test test-short lint fmt vet tidy clean

GO := go
GOFLAGS := -trimpath
LDFLAGS := -s -w

# VERSION is stamped into the binary via -ldflags. Override in CI with a
# release tag; default to `git describe` so local builds stay traceable to
# a commit.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

BIN_DIR := bin

all: build

build:
	$(GO) build $(GOFLAGS) \
		-ldflags '$(LDFLAGS) -X main.version=$(VERSION) -X main.commit=$(COMMIT)' \
		-o $(BIN_DIR)/incuse ./cmd/incuse

test:
	$(GO) test -race -count=1 ./...

test-short:
	$(GO) test -short -race -count=1 ./...

lint:
	golangci-lint run ./...

fmt:
	$(GO) fmt ./...
	goimports -w .

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(BIN_DIR)
