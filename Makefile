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
# A path as one shell word, whatever quotes it holds.
shq = '$(subst ','\'',$(1))'

.PHONY: build helpers completions install test vet e2e clean

# CGO off, as in the release builds (.goreleaser.yaml), so a local binary
# behaves like the one users install.
build: helpers
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/spotexec ./cmd/spotexec
	ln -sf spotexec bin/sx

# Cross-compile the remote helper for every supported runner platform and
# gzip it into the embed directory picked up by the main binary. Each helper
# is built and compressed in a hidden staging directory (go:embed skips it),
# its stream is tested, and only then is it renamed into place, so a failed
# build or compressor fails the target and leaves the last good helper alone.
helpers:
	@embed_dir=$(call shq,$(EMBED_DIR)); \
	case "$$embed_dir" in -*) embed_dir=./$$embed_dir ;; esac; \
	mkdir -p "$$embed_dir" || exit 1; \
	tmp=; \
	trap 'if [ -n "$$tmp" ]; then rm -rf "$$tmp"; fi' 0; \
	trap 'exit 1' HUP INT TERM; \
	tmp=$$(mktemp -d "$$embed_dir/.tmp-helper.XXXXXXXX") || exit 1; \
	for t in $(HELPER_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		out="$$embed_dir/spotexec-helper-$$os-$$arch.gz"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o "$$tmp/helper" ./cmd/spotexec-helper || exit 1; \
		gzip -9 -n -c "$$tmp/helper" > "$$tmp/helper.gz" || { printf 'helper %s: gzip failed\n' "$$t" >&2; exit 1; }; \
		gzip -t "$$tmp/helper.gz" || { printf 'helper %s: gzip wrote a corrupt stream\n' "$$t" >&2; exit 1; }; \
		[ ! -d "$$out" ] || { printf '%s is a directory\n' "$$out" >&2; exit 1; }; \
		mv -f "$$tmp/helper.gz" "$$out" || exit 1; \
		printf 'helper %s\n' "$$t"; \
	done

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
	install -d $(call shq,$(BINDIR))
	install -m 0755 bin/spotexec $(call shq,$(BINDIR)/spotexec)
	ln -sf spotexec $(call shq,$(BINDIR)/sx)

test:
	go test ./...

vet:
	go vet ./...

e2e: build
	./scripts/e2e.sh
	./scripts/e2e-pool.sh
	./scripts/e2e-daemon.sh

clean:
	rm -rf bin/spotexec bin/sx completions $(EMBED_DIR)/spotexec-helper-*.gz $(EMBED_DIR)/.tmp-helper*
