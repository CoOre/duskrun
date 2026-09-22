BIN := ./bin/duskrun
PKG := ./...
ARGS ?=
IMAGE ?= duskrun:latest

# Stamped into the binary so `duskrun version` and the settings page report the
# build rather than a hardcoded constant. Falls back to the commit when no tag
# exists yet; `-dirty` marks a build made from uncommitted changes, which is the
# one you most need to be able to tell apart.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo unknown)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test vet lint vuln check snapshot run tidy clean docker dist ui test-ui release

# Go binary. Run `make ui` first (or just `make release`) to embed the real SPA.
# Without it the build still succeeds without a Node toolchain, but the binary
# serves internal/web/placeholder.html, which says so on every page rather than
# rendering blank.
build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/duskrun

# Build the SPA into internal/web/dist so `go build`/`go:embed` ships the real UI.
ui:
	cd web && npm ci && npm run build

# Frontend unit tests (jsdom).
test-ui:
	cd web && npm ci && npm run test -- --run

# Full release build: frontend then the embedding Go binary.
release: ui build

test:
	go test $(PKG)

vet:
	go vet $(PKG)

# Те же проверки, что в CI (.github/workflows/ci.yml). Версии линтера и
# goreleaser совпадают с workflow; инструменты не нужно ставить заранее.
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2
GOVULNCHECK   := go run golang.org/x/vuln/cmd/govulncheck@latest
GORELEASER    := go run github.com/goreleaser/goreleaser/v2@v2.18.2

lint:
	$(GOLANGCI_LINT) run $(PKG)

vuln:
	$(GOVULNCHECK) $(PKG)

# Всё, что CI гоняет для Go и фронта, одной командой перед пушем.
check: vet lint vuln
	go mod tidy -diff
	go test -race $(PKG)
	cd web && npm ci && npm run typecheck && npm run test -- --run

# Локальный прогон релиза без публикации: архивы и checksums в ./dist.
snapshot:
	$(GORELEASER) release --snapshot --clean

run:
	go run ./cmd/duskrun $(ARGS)

tidy:
	go mod tidy

clean:
	rm -rf ./bin ./dist

# Build the container image (Node stage builds UI; runtime carries pg_dump/mysqldump).
docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

# Produce a stripped static binary under dist/ (embeds whatever is in internal/web/dist).
dist:
	mkdir -p dist
	CGO_ENABLED=0 go build -ldflags "-s -w $(LDFLAGS)" -o dist/duskrun ./cmd/duskrun
