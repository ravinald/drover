BIN     := drover
PKG     := github.com/ravinald/drover
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(PKG)/internal/cmd.version=$(VERSION) \
	-X $(PKG)/internal/cmd.commit=$(COMMIT) \
	-X $(PKG)/internal/cmd.date=$(DATE)

# The release matrix, and the same list CI cross-compiles on every pull request,
# so a target that stopped building never reaches a tag.
TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: build install dist test lint vuln check clean

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BIN) ./cmd/$(BIN)

install:
	go install -trimpath -ldflags '$(LDFLAGS)' ./cmd/$(BIN)

# The artifacts a tag publishes, stamped from the tree they were cut from. Run
# it on a clean checkout of the tag: VERSION reads `git describe --dirty`, so a
# stray edit ends up in every filename and in every binary's `drover version`.
dist:
	rm -rf dist && mkdir -p dist
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		out=dist/$(BIN)_$(VERSION)_$${os}_$${arch}; \
		if [ "$$os" = windows ]; then out=$$out.exe; fi; \
		echo "==> $$out"; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o $$out ./cmd/$(BIN) || exit 1; \
	done
	cd dist && shasum -a 256 $(BIN)_* > checksums.txt

# internal/cmd takes 436-573s on an idle host, and `drive --jobs 2` runs gates
# side by side, so under load it overruns go test's 10-minute default and the
# timeout panic reads as a test failure. A caller's TEST_TIMEOUT=... or
# GOFLAGS=-timeout=... still wins: go applies GOFLAGS before the command line,
# so the flag is left off whenever GOFLAGS already names a timeout.
TEST_TIMEOUT ?= 20m
TIMEOUT_FLAG := $(if $(findstring timeout,$(GOFLAGS)),,-timeout=$(TEST_TIMEOUT))

test:
	go test $(TIMEOUT_FLAG) ./...
# `go test` keys its cache on the test binary and the files the test opens; a
# child process's inputs are invisible to it, so the `git tag` this one reads is
# not a cache input. A tag created after a passing run returns that run's
# verdict. Blanket -count=1 costs the whole suite, so re-run just this one.
	go test -count=1 -run TestSupportedVersionsTableNamesTheCurrentMinor $(TIMEOUT_FLAG) ./internal/cmd/

lint:
	golangci-lint run

vuln:
	govulncheck ./...

# The example project ships, so its own check belongs to the gate rather than to
# a CI step beside it: kept out of the gate, a finding that fires only there is
# one a green local run cannot see.
#
# Under an empty XDG root, because whether examples/ passes must not depend on
# the per-user config of whoever runs it. DROVER_ROOT is cleared for the same
# reason: inside a drover session it names that session's project, and config
# lookup reads it before walking up from examples/.
example: build
	@DROVER_ROOT= XDG_CONFIG_HOME=$$(mktemp -d) sh -c 'cd examples && ../bin/$(BIN) check'

check: lint test vuln example

clean:
	rm -rf bin dist
