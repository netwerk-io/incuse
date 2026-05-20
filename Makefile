.PHONY: all build test test-short lint fmt vet tidy clean install-remote release-snapshot

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
	rm -rf $(BIN_DIR) dist

# release-snapshot runs GoReleaser locally without publishing — useful for
# verifying .goreleaser.yaml after changes. Produces dist/*.tar.gz and
# SHA256SUMS the same way the tagged-release pipeline does.
release-snapshot:
	goreleaser release --snapshot --clean --skip=publish

# install-remote builds a linux/amd64 binary, ships it to a remote host
# via scp, runs deploy/systemd/install.sh, and restarts the service.
# Local-dev convenience target — production should consume the release
# tarball, not this.
#
#   make install-remote HOST=<hostname>
INSTALL_HOST ?= $(HOST)
INSTALL_USER ?= root
install-remote:
	@if [ -z "$(INSTALL_HOST)" ]; then echo "set HOST=<hostname>"; exit 1; fi
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) \
		-ldflags '$(LDFLAGS) -X main.version=$(VERSION) -X main.commit=$(COMMIT)' \
		-o $(BIN_DIR)/incuse-linux-amd64 ./cmd/incuse
	scp $(BIN_DIR)/incuse-linux-amd64 deploy/systemd/incuse.service deploy/systemd/incuse.example.yaml deploy/systemd/install.sh $(INSTALL_USER)@$(INSTALL_HOST):/tmp/incuse-install/
	ssh $(INSTALL_USER)@$(INSTALL_HOST) 'cd /tmp/incuse-install && bash install.sh ./incuse-linux-amd64 && systemctl restart incuse'
