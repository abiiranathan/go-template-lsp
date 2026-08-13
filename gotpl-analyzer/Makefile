# ─────────────────────────────────────────────────────────────────────────────
# Go Template LSP Analyzer Makefile
# ─────────────────────────────────────────────────────────────────────────────

MODULE       := github.com/abiiranathan/go-template-lsp/gotpl-analyzer
BINARY_NAME  := gotpl-analyzer
WINDOWS_BIN  := $(BINARY_NAME).exe

# Extract version from git tag, falling back to v0.1.0-dev if untagged
VERSION      ?= $(shell git describe --tags --exact-match 2>/dev/null || git describe --tags 2>/dev/null || echo "v0.1.0-dev")
COMMIT       := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE         := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

# Inject version metadata into binary at link time
LDFLAGS      := -s -w \
                -X 'main.Version=$(VERSION)' \
                -X 'main.GitCommit=$(COMMIT)' \
                -X 'main.BuildDate=$(DATE)'

GOPATH_BIN   := $(shell go env GOPATH)/bin

.PHONY: all build build-all install test clean tag release bump-patch bump-minor bump-major

all: test build

## build: Build binary for current platform with version embedded
build:
	@echo "==> Building $(BINARY_NAME) $(VERSION) ($(COMMIT))..."
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY_NAME) .

## install: Install binary to $GOPATH/bin
install:
	@echo "==> Installing $(BINARY_NAME) to $(GOPATH_BIN)..."
	go install -trimpath -ldflags "$(LDFLAGS)" .

## test: Run unit and integration tests
test:
	@echo "==> Running tests..."
	go test -v -race -cover ./...

## clean: Remove built binaries
clean:
	@echo "==> Cleaning artifacts..."
	@rm -f $(BINARY_NAME) $(WINDOWS_BIN)

# ── Cross-Compilation ────────────────────────────────────────────────────────

## build-all: Cross-compile binaries for Linux, macOS, and Windows
build-all: clean
	@echo "==> Cross-compiling for multiple platforms..."
	@mkdir -p dist
	GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY_NAME)-linux-amd64 .
	GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY_NAME)-linux-arm64 .
	GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY_NAME)-darwin-amd64 .
	GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY_NAME)-darwin-arm64 .
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY_NAME)-windows-amd64.exe .
	@echo "==> Binaries saved to ./dist/"

# ── Version Bumping & Deployment ─────────────────────────────────────────────

# Helper function to compute next SemVer version
CURRENT_TAG := $(shell git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
V_NUMBERS   := $(subst ., ,$(subst v,,$(CURRENT_TAG)))
MAJOR       := $(word 1,$(V_NUMBERS))
MINOR       := $(word 2,$(V_NUMBERS))
PATCH       := $(word 3,$(V_NUMBERS))

bump-patch:
	$(eval NEW_TAG := v$(MAJOR).$(MINOR).$(shell echo $$(($(PATCH)+1))))
	@echo "Next patch version: $(NEW_TAG)"

bump-minor:
	$(eval NEW_TAG := v$(MAJOR).$(shell echo $$(($(MINOR)+1))).0)
	@echo "Next minor version: $(NEW_TAG)"

bump-major:
	$(eval NEW_TAG := v$(shell echo $$(($(MAJOR)+1))).0.0)
	@echo "Next major version: $(NEW_TAG)"

## release: Publish a new release tag and warm up Go module proxy
## Usage: make release TAG=v0.2.0   or   make bump-patch release
release:
	@if [ -z "$(TAG)" ] && [ -z "$(NEW_TAG)" ]; then \
		echo "ERROR: Please specify TAG (e.g. make release TAG=v0.2.0) or use bump-* targets"; \
		exit 1; \
	fi
	$(eval TARGET_TAG := $(if $(TAG),$(TAG),$(NEW_TAG)))
	@echo "==> Checking git working tree..."
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "ERROR: Working tree is dirty. Commit or stash changes before releasing."; \
		exit 1; \
	fi
	@echo "==> Creating Git tag $(TARGET_TAG)..."
	git tag -a $(TARGET_TAG) -m "Release $(TARGET_TAG)"
	@echo "==> Pushing tag to origin..."
	git push origin $(TARGET_TAG)
	@echo "==> Warming up Go module proxy cache (proxy.golang.org)..."
	@GOPROXY=https://proxy.golang.org go list -m $(MODULE)@$(TARGET_TAG) || true
	@echo ""
	@echo "✓ Successfully deployed $(TARGET_TAG)!"
	@echo "Users can now update with: go install $(MODULE)@latest"
	