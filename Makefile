.DEFAULT_GOAL := help
GO ?= go
BIN := bin/ozymandis

# The Tailwind standalone CLI is a single binary with no Node dependency.
# `make tailwind` fetches it into ./bin so a contributor needs nothing
# installed beyond Go.
TAILWIND_VERSION ?= v4.3.3
TAILWIND ?= bin/tailwindcss
UNAME_S := $(shell uname -s | tr '[:upper:]' '[:lower:]')
UNAME_M := $(shell uname -m)
ifeq ($(UNAME_M),x86_64)
	TW_ARCH := x64
else
	TW_ARCH := arm64
endif
TAILWIND_URL := https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/tailwindcss-$(UNAME_S)-$(TW_ARCH)

CSS_IN  := assets/css/input.css
CSS_OUT := internal/web/assets/css/app.css

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

$(TAILWIND):
	@mkdir -p bin
	curl -sSL -o $(TAILWIND) $(TAILWIND_URL)
	chmod +x $(TAILWIND)

.PHONY: tailwind
tailwind: $(TAILWIND) ## Download the Tailwind standalone CLI

.PHONY: css
css: $(TAILWIND) ## Compile the stylesheet
	$(TAILWIND) -i $(CSS_IN) -o $(CSS_OUT) --minify

.PHONY: css-watch
css-watch: $(TAILWIND) ## Recompile the stylesheet on change
	$(TAILWIND) -i $(CSS_IN) -o $(CSS_OUT) --watch

.PHONY: generate
generate: ## Run templ codegen
	$(GO) tool templ generate

.PHONY: assets
assets: generate css ## Regenerate templates and stylesheet

.PHONY: gallery
gallery: assets ## Render every visual state to HTML
	OZYMANDIS_GALLERY_OUT=$(or $(OUT),/tmp/ozymandis-gallery) $(GO) test ./internal/web -run Gallery -count=1
	@echo
	@echo "  Serve it — the pages ask for /assets, so file:// renders unstyled:"
	@echo "    cd $(or $(OUT),/tmp/ozymandis-gallery) && python3 -m http.server 8123"

.PHONY: build
build: assets oz ## Build both binaries
	CGO_ENABLED=0 $(GO) build -o $(BIN) ./cmd/ozymandis

# The CLI, built separately because it shares almost nothing with the server.
#
# It depends on the TOML decoder and internal/appspec and nothing else — no
# database driver, no Kubernetes client — which is why appspec validates only
# the shape of a file and leaves every semantic rule to the server. Keep it that
# way: `go list -deps ./cmd/oz` should stay short enough to read.
#
# No `assets` dependency: the CLI renders no templates, so making it wait on
# Tailwind would be a minute of nothing on every build.
.PHONY: oz
oz: ## Build the oz CLI
	CGO_ENABLED=0 $(GO) build -o bin/oz ./cmd/oz

.PHONY: test
test: ## Run tests
	$(GO) test ./... -race -count=1

# The backup round-trip needs storage and a database to run against, so it
# skips without them — and a skip reads as a pass. This target provides them,
# runs the real restic scripts against real infrastructure, and takes the whole
# lot down again.
#
# It is the only test that proves a backup can be restored. Everything else
# about the backup path is checked without a cluster; this is the part that
# cannot be.
BACKUP_NET  := oz-backup-test
BACKUP_IMG  := ozymandis-backup:test

.PHONY: test-backup
test-backup: ## Run the backup/restore round-trip against MinIO and Postgres
	docker build -t $(BACKUP_IMG) build/backup
	-docker network create $(BACKUP_NET)
	-docker rm -f oz-minio oz-pg-src
	docker run -d --name oz-minio --network $(BACKUP_NET) \
		-e MINIO_ROOT_USER=ozminio -e MINIO_ROOT_PASSWORD=ozminio123 \
		minio/minio:latest server /data
	docker run -d --name oz-pg-src --network $(BACKUP_NET) \
		-e POSTGRES_USER=ozymandis -e POSTGRES_PASSWORD=pgsecret \
		-e POSTGRES_DB=ozymandis postgres:16-alpine
	@echo "waiting for minio and postgres"
	@sleep 8
	docker run --rm --network $(BACKUP_NET) --entrypoint sh minio/mc:latest -c \
		"mc alias set m http://oz-minio:9000 ozminio ozminio123 && \
		 mc mb -p m/ozymandis-backups"
	OZYMANDIS_TEST_S3_ENDPOINT=http://oz-minio:9000 \
	OZYMANDIS_TEST_BACKUP_IMAGE=$(BACKUP_IMG) \
		$(GO) test ./internal/backup -count=1 -v
	-docker rm -f oz-minio oz-pg-src
	-docker network rm $(BACKUP_NET)

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: check
check: vet test ## Vet + test

.PHONY: run
run: build ## Run locally
	$(BIN)

.PHONY: dev
dev: assets ## Rebuild and run
	$(GO) run ./cmd/ozymandis

.PHONY: tidy
tidy: ## Tidy modules
	$(GO) mod tidy

.PHONY: sqlc
sqlc: ## Regenerate database code from SQL
	$(GO) tool sqlc generate

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin dist
