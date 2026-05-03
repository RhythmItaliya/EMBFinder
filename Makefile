# ─────────────────────────────────────────────────────────────────────────────
# EMBFinder — Makefile
#
# Usage:
#   make dev          — run all three services locally (dev mode)
#   make build        — headless Linux binary (CGO_ENABLED=0, static)
#   make build-all    — build for Linux + Windows + macOS via GoReleaser
#   make release      — create a production release (requires VERSION=v1.0.0)
#   make docker       — build all Docker images
#   make docker-up    — start all services with Docker Compose
#   make test         — run quick accuracy test
#   make clean        — remove built artifacts
# ─────────────────────────────────────────────────────────────────────────────

.PHONY: dev build build-all release docker docker-up test clean help

# ── Configuration ─────────────────────────────────────────────────────────────
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_DATE := $(shell date -u +%Y-%m-%d)
LDFLAGS    := -s -w -X main.version=$(VERSION) -X main.buildDate=$(BUILD_DATE)
GO_SERVER  := ./go-server
DIST       := ./dist

# ── Default target ────────────────────────────────────────────────────────────
help:
	@echo ""
	@echo "  EMBFinder $(VERSION)"
	@echo ""
	@echo "  make dev          Start all services locally (development)"
	@echo "  make build        Build headless Linux binary"
	@echo "  make build-all    Cross-compile all platforms (GoReleaser snapshot)"
	@echo "  make release      Tag + publish release  (VERSION=v1.2.3)"
	@echo "  make docker       Build Docker images"
	@echo "  make docker-up    Start all services via Docker Compose"
	@echo "  make test         Run quick accuracy test"
	@echo "  make clean        Remove build artifacts"
	@echo ""

# ── Development ───────────────────────────────────────────────────────────────
dev:
	@echo "Starting EMBFinder in development mode..."
	@echo ""
	@echo "  1. EMB Engine  → http://localhost:8767"
	@echo "  2. Embedder    → http://localhost:8766"
	@echo "  3. Go Server   → http://localhost:8765"
	@echo ""
	@echo "Opening three terminals..."
	@(cd emb-engine && python3 server.py) &
	@sleep 2
	@(cd embedder && python3 -m uvicorn main:app --host 127.0.0.1 --port 8766 --workers 1) &
	@sleep 2
	@(cd go-server && MODE=development go run -tags dev .)

# ── Local binary builds ───────────────────────────────────────────────────────
build:
	@echo "Building headless Linux binary..."
	@mkdir -p $(DIST)
	cd $(GO_SERVER) && CGO_ENABLED=0 go build -tags headless \
		-ldflags="$(LDFLAGS)" \
		-o ../$(DIST)/embfinder .
	@echo "  → $(DIST)/embfinder  ($(shell du -sh $(DIST)/embfinder | cut -f1))"

build-windows:
	@echo "Building Windows binary..."
	@mkdir -p $(DIST)
	cd $(GO_SERVER) && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -tags headless \
		-ldflags="$(LDFLAGS)" \
		-o ../$(DIST)/embfinder.exe .
	@echo "  → $(DIST)/embfinder.exe"

build-mac-intel:
	@echo "Building macOS Intel binary..."
	@mkdir -p $(DIST)
	cd $(GO_SERVER) && GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -tags headless \
		-ldflags="$(LDFLAGS)" \
		-o ../$(DIST)/embfinder-darwin-amd64 .
	@echo "  → $(DIST)/embfinder-darwin-amd64"

build-mac-arm:
	@echo "Building macOS Apple Silicon binary..."
	@mkdir -p $(DIST)
	cd $(GO_SERVER) && GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -tags headless \
		-ldflags="$(LDFLAGS)" \
		-o ../$(DIST)/embfinder-darwin-arm64 .
	@echo "  → $(DIST)/embfinder-darwin-arm64"

# Build all platforms via GoReleaser (creates dist/ with archives + packages)
build-all:
	@echo "Building all platforms with GoReleaser (snapshot)..."
	@which goreleaser > /dev/null 2>&1 || \
		(echo "Install: go install github.com/goreleaser/goreleaser/v2@latest" && exit 1)
	goreleaser release --snapshot --clean
	@echo ""
	@echo "Artifacts in $(GO_SERVER)/dist/"

# ── Production release ────────────────────────────────────────────────────────
release:
ifndef VERSION
	$(error VERSION is required: make release VERSION=v1.0.0)
endif
	@echo "Tagging $(VERSION) and pushing..."
	git tag $(VERSION)
	git push origin $(VERSION)
	@echo ""
	@echo "GitHub Actions will now build and publish the release automatically."
	@echo "Watch progress at: https://github.com/RhythmItaliya/EMBFinder/actions"

# ── Docker ───────────────────────────────────────────────────────────────────
docker:
	@echo "Building Docker images..."
	docker compose build --parallel

docker-up:
	@echo "Starting all services with Docker Compose..."
	docker compose up -d
	@echo ""
	@echo "  EMBFinder UI  → http://localhost:8765"
	@echo "  Embedder API  → http://localhost:8766/health"
	@echo "  EMB Engine    → http://localhost:8767/health"
	@echo ""
	@echo "  Logs: docker compose logs -f"
	@echo "  Stop: docker compose down"

docker-down:
	docker compose down

# ── Testing ───────────────────────────────────────────────────────────────────
test:
	@echo "Running quick accuracy test..."
	python3 tests/quick_test.py --skip-index

test-full:
	@echo "Running full mega test (re-indexes library)..."
	python3 tests/mega_test.py

# ── Cleanup ───────────────────────────────────────────────────────────────────
clean:
	rm -rf $(DIST) $(GO_SERVER)/dist
	find . -name "__pycache__" -type d -exec rm -rf {} + 2>/dev/null || true
	find . -name "*.pyc" -delete 2>/dev/null || true
	@echo "Cleaned."
