# Build, test and package evsectl.
#
#   make                 static binary for the EVSE (linux/amd64)
#   make test            run all tests
#   make dist            evse-controller-VERSION.tar.gz in dist/
#   make sim             run with simulated boards on :8080
#
# VERSION defaults to `git describe` (tag v0.4.0 -> 0.4.0); override with
# `make dist VERSION=0.4.0`.

VERSION ?= $(patsubst v%,%,$(shell git describe --tags --always --dirty 2>/dev/null || echo dev))

GO      ?= go
BIN     := evsectl
PKG     := evse-controller-$(VERSION)
DISTDIR := dist

export GOTOOLCHAIN := local

BUILD_ENV   := CGO_ENABLED=0 GOOS=linux GOARCH=amd64
BUILD_FLAGS := -trimpath -ldflags "-s -w -X main.version=$(VERSION)"

# Files shipped next to the binary. install.sh expects evsectl one level
# above deploy/.
DIST_FILES := README.md docs/screenshot.png $(wildcard deploy/*)

.PHONY: all build test vet sim dist clean

all: build

# Always rebuilt: go's own cache makes this cheap, and it keeps the embedded
# version and assets current.
build:
	$(BUILD_ENV) $(GO) build $(BUILD_FLAGS) -o $(BIN) ./cmd/evsectl

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

sim:
	$(GO) run ./cmd/evsectl -simulate -config deploy/config.example.yaml -data-dir ./data

dist: build
	rm -rf $(DISTDIR)/$(PKG) $(DISTDIR)/$(PKG).tar.gz
	mkdir -p $(DISTDIR)/$(PKG)
	cp -p $(BIN) $(DISTDIR)/$(PKG)/
	tar -cf - $(DIST_FILES) | tar -xf - -C $(DISTDIR)/$(PKG)
	tar -C $(DISTDIR) --sort=name --owner=0 --group=0 --numeric-owner \
		-czf $(DISTDIR)/$(PKG).tar.gz $(PKG)
	rm -rf $(DISTDIR)/$(PKG)
	@echo "Created $(DISTDIR)/$(PKG).tar.gz"

clean:
	rm -rf $(BIN) $(DISTDIR)
