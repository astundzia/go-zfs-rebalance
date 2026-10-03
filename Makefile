# go-zfs-rebalance build tasks (GNU make on macOS or Linux).
#   make            build bin/rebalance for this computer
#   make test       run all tests with the race detector (on Linux this needs gcc or clang)
#   make lint       vet, staticcheck and shellcheck
#   make dist       release binaries, checksums and install.sh in dist/
#   make help       list every target

BINARY  := rebalance
PKG     := ./cmd/rebalance
GO      ?= go
BINDIR  ?= /usr/local/bin

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# staticcheck 2026.2.1. Keep in sync with .github/workflows/ci.yml.
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.1

# Release targets. Only Linux and macOS are supported.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

# Both print "<hash>  <name>", the format the installer and `sha256sum -c` expect.
SHA256 := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo 'shasum -a 256')

# Release files must be built with at least the Go named on go.mod's toolchain line: older
# Go releases have security bugs in os.Root, which rebalance relies on to stay inside the folder.
MIN_GO := $(shell awk '$$1 == "toolchain" { print $$2 }' go.mod)

.PHONY: build test test-short lint dist install clean help

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(PKG)

test:
	@if [ "$$($(GO) env GOOS)" = linux ] && [ "$$($(GO) env CGO_ENABLED)" != 1 ]; then \
		echo "The race detector needs a C compiler on Linux. Please install gcc (or clang) and try again,"; \
		echo "or run 'make test-short' to test without it."; \
		exit 1; \
	fi
	$(GO) test -race ./...

test-short:
	$(GO) test -short ./...

lint:
	$(GO) vet ./...
	GOOS=linux GOARCH=amd64 $(GO) vet ./...
	GOOS=linux GOARCH=arm64 $(GO) vet ./...
	GOOS=darwin GOARCH=arm64 $(GO) vet ./...
	GOOS=windows GOARCH=amd64 $(GO) vet ./...
	$(GO) run $(STATICCHECK) ./...
	shellcheck -s sh install.sh
	shellcheck test/lab/lab.sh

dist:
	rm -rf dist
	mkdir -p dist
	@set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; name=$(BINARY)-$$os-$$arch; \
		echo "building dist/$$name ($(VERSION))"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/$$name $(PKG); \
		(cd dist && $(SHA256) $$name > $$name.sha256); \
		v=$$($(GO) version dist/$$name | awk '{ print $$2 }'); \
		if [ "$$(printf '%s\n' $(MIN_GO) $$v | sort -V | head -n 1)" != "$(MIN_GO)" ]; then \
			echo "dist/$$name was built with $$v, but release files need $(MIN_GO) or newer,"; \
			echo "which fixes security bugs in os.Root. Please update Go, or unset GOTOOLCHAIN=local."; \
			exit 1; \
		fi; \
	done
	cp install.sh dist/install.sh
	chmod 755 dist/install.sh
	cd dist && $(SHA256) $(foreach p,$(PLATFORMS),$(BINARY)-$(subst /,-,$(p))) install.sh > checksums.txt
	@echo "Release files are ready in dist/:"
	@cat dist/checksums.txt

install:
	@test -f bin/$(BINARY) || { echo "Please run 'make build' first (without sudo), then 'sudo make install'."; exit 1; }
	install -d $(DESTDIR)$(BINDIR)
	install -m 0755 bin/$(BINARY) $(DESTDIR)$(BINDIR)/$(BINARY)
	@echo "Installed $(BINDIR)/$(BINARY)."
	@echo "Next, see how evenly a pool is filled (this changes nothing):"
	@echo "  sudo $(BINDIR)/$(BINARY) --report /mnt/tank/media"

clean:
	rm -rf bin dist

help:
	@echo "make build       build bin/$(BINARY) for this computer"
	@echo "make test        run all tests with the race detector"
	@echo "make test-short  run the quick tests only"
	@echo "make lint        go vet (this OS, Linux, macOS, Windows), staticcheck, shellcheck"
	@echo "make dist        build release files into dist/ (set VERSION=v2.0.0)"
	@echo "make install     copy bin/$(BINARY) to $(BINDIR) (use sudo)"
	@echo "make clean       remove bin/ and dist/"
