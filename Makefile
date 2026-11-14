VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PKG     := github.com/daintreehq/spotexec
LDFLAGS := -s -w -X $(PKG)/internal/cli.Version=$(VERSION) -X $(PKG)/internal/helper.Version=$(VERSION)
HELPER_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
EMBED_DIR := internal/helperbin/dist

.PHONY: build helpers install test vet e2e clean

build: helpers
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/spotexec ./cmd/spotexec
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

install: build
	install -m 0755 bin/spotexec $(shell go env GOPATH)/bin/spotexec
	ln -sf spotexec $(shell go env GOPATH)/bin/sx

test:
	go test ./...

vet:
	go vet ./...

e2e: build
	./scripts/e2e.sh
	./scripts/e2e-pool.sh
	./scripts/e2e-daemon.sh

clean:
	rm -f bin/spotexec bin/sx $(EMBED_DIR)/spotexec-helper-*.gz
