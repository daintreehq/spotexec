# The tag without its "v" (0.2.0, 0.2.0-3-gabc1234-dirty), as GoReleaser
# embeds it, so every build prints its version the same way.
VERSION ?= $(patsubst v%,%,$(shell git describe --tags --always --dirty 2>/dev/null || echo dev))
PKG     := github.com/daintreehq/spotexec
LDFLAGS := -s -w -X $(PKG)/internal/cli.Version=$(VERSION) -X $(PKG)/internal/helper.Version=$(VERSION)
HELPER_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
EMBED_DIR := internal/helperbin/dist
# Where `make install` puts spotexec and sx: GOBIN, else the first GOPATH
# entry's bin, as `go install` would. BINDIR=... overrides both.
BINDIR ?= $(shell b=$$(go env GOBIN); [ -n "$$b" ] || b="$$(go env GOPATH | cut -d: -f1)/bin"; printf '%s' "$$b")

.PHONY: build helpers completions install test vet e2e clean

# CGO off, as in the release builds (.goreleaser.yaml), so a local binary
# behaves like the one users install.
build: helpers
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/spotexec ./cmd/spotexec
	ln -sf spotexec bin/sx

# Cross-compile the remote helper for every supported runner platform and
# gzip it into the embed directory picked up by the main binary.
helpers:
	@mkdir -p $(EMBED_DIR)
	@for t in $(HELPER_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o $(EMBED_DIR)/.tmp-helper ./cmd/spotexec-helper || exit 1; \
		gzip -9 -n -c $(EMBED_DIR)/.tmp-helper > $(EMBED_DIR)/spotexec-helper-$$os-$$arch.gz; \
		echo "helper $$os/$$arch"; \
	done
	@rm -f $(EMBED_DIR)/.tmp-helper

# Shell completion scripts for spotexec and sx, shipped in the release
# archives, the Homebrew formula and the Linux packages.
completions:
	@rm -rf completions && mkdir -p completions
	go build -o completions/.gen ./cmd/spotexec
	@for sh in bash zsh fish; do \
		./completions/.gen completion $$sh > completions/spotexec.$$sh || exit 1; \
		./completions/.gen completion sx $$sh > completions/sx.$$sh || exit 1; \
	done
	@rm -f completions/.gen

install: build
	install -d '$(BINDIR)'
	install -m 0755 bin/spotexec '$(BINDIR)/spotexec'
	ln -sf spotexec '$(BINDIR)/sx'

test:
	go test ./...

vet:
	go vet ./...

e2e: build
	./scripts/e2e.sh
	./scripts/e2e-pool.sh
	./scripts/e2e-daemon.sh

clean:
	rm -rf bin/spotexec bin/sx completions $(EMBED_DIR)/spotexec-helper-*.gz
