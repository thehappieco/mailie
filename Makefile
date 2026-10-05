.PHONY: help build run test test-race cover fuzz vet fmt lint-layout lint check it tidy clean \
	backup-tool image public-source web-install web-dev web-build web-test web-check

GO   ?= go
GOLANGCI_LINT ?= golangci-lint
PKGS := ./...
BIN  := bin
NPM  ?= npm
WEB  := web

export CGO_ENABLED = 0
# The race detector needs cgo on every platform but macOS (Go 1.20 and later),
# so the two race runs below, and only they, turn it on there. Nothing they
# build ships: every binary is built with CGO_ENABLED=0.
RACE_CGO = $(if $(filter darwin,$(shell $(GO) env GOOS)),0,1)

help:
	@grep -E '^[a-z-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

build: ## Build the binary (pure Go, no cgo)
	$(GO) build -o $(BIN)/ ./cmd/...

run: build ## Build and run the daemon (reads .env)
	./$(BIN)/mailserver serve

test: ## Run the unit tier
	$(GO) test $(PKGS)

test-race: ## Run the unit tier with the race detector
	CGO_ENABLED=$(RACE_CGO) $(GO) test -race $(PKGS)

cover: ## Report coverage
	CGO_ENABLED=$(RACE_CGO) $(GO) test -race -coverprofile=coverage.out $(PKGS)
	$(GO) tool cover -func=coverage.out | tail -1

fuzz: ## Short fuzz pass over every fuzz target (the MIME parser lives or dies here)
	@for p in $$($(GO) list $(PKGS)); do \
	  for f in $$($(GO) test -list='Fuzz.*' $$p 2>/dev/null | grep '^Fuzz' || true); do \
	    echo "--- $$p $$f"; \
	    $(GO) test -run=NONE -fuzz=$$f -fuzztime=10s $$p || exit 1; \
	  done; \
	done

vet: ## go vet
	$(GO) vet $(PKGS)

# web/ is the open console, a Node project with its own check (web-check).
# commercial/, where a development checkout nests the private cloud, is a
# module of its own with its own Makefile. Nothing in either is Go this module
# owns, least of all node_modules, so fmt and lint-layout skip them — as
# go.mod's ignore and commercial/'s own go.mod do for ./...
fmt: ## Check formatting
	@out=$$(find . \( -path ./$(WEB) -o -path ./commercial -o -path ./.git \) -prune -o -name '*.go' -print | xargs gofmt -l); \
	if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

# A half-finished migration that leaves a parallel copy of a package beside the
# real one is how a codebase stops being readable. Fail the build instead.
lint-layout: ## Reject parallel legacy package copies
	@bad=$$(find . \( -path ./$(WEB) -o -path ./commercial -o -path ./.git \) -prune -o -type d \( -name '*_ttt' -o -name '*_old' -o -name '*_bak' -o -name '* copy' \) -print); \
	if [ -n "$$bad" ]; then echo "legacy package copies are not allowed:"; echo "$$bad"; exit 1; fi

check: fmt vet lint-layout test-race ## Everything CI runs for the unit tier

# The integration tag too, so the integration tier's files are linted.
lint: ## Run golangci-lint over every package, the integration tier's included
	$(GOLANGCI_LINT) run --build-tags integration $(PKGS)

# The integration tier needs Docker: Dovecot speaks CONDSTORE and SPECIAL-USE,
# which the in-process fake server cannot, and Mailpit captures real SMTP.
it: ## Run the integration tier against docker compose
	docker compose -f it/compose.yml up --wait
	$(GO) test -tags integration -count=1 $(PKGS) || (docker compose -f it/compose.yml down -v; exit 1)
	docker compose -f it/compose.yml down -v

tidy: ## Tidy modules
	$(GO) mod tidy

# A static linux/amd64 binary for running a restore on another machine, where
# the backups may be decrypted and the restored database, which is in clear,
# may be kept (docs/backup.md, "Restoring").
backup-tool: ## Build dist/mailserver-linux-amd64 for a restore on another machine
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath \
	  -ldflags "-s -w -X main.version=$$(git describe --always --dirty)" -o dist/mailserver-linux-amd64 ./cmd/mailserver

# The self-hosting image: the daemon and the open console, built by Docker
# from this checkout (deploy/Dockerfile; docs/self-hosting.md). It needs
# neither Go nor Node here.
image: ## Build the self-hosting image mailie:local with Docker
	docker build -f deploy/Dockerfile --build-arg VERSION=$$(git describe --always --dirty) -t mailie:local .

# The public source: an allowlist of this checkout, exactly what Git tracks
# there, without commercial/, history, dependencies or builds, refused if a
# file is untracked or ignored, or holds a private name
# (scripts/export-public.py). CI builds and tests the snapshot on its own.
public-source: ## Export the allowlisted public source snapshot to dist/mailie-source.tar.gz
	python3 scripts/export-public.py --output dist/mailie-source.tar.gz

# The console. Deliberately not part of check: the Go tiers must never need
# Node, and CI runs these in a job of its own. web-* is the open console
# (web/), what a self-hosted server serves; another edition's targets live
# with it. Every edition's dev server takes localhost:5174, the port the
# OAuth web client's redirect names: run one at a time. The open console
# wants the daemon of make run.
web-install: ## Install the open console's dependencies exactly as locked
	cd $(WEB) && $(NPM) ci

web-dev: ## Serve the open console on localhost:5174, proxying /v1 and /mcp to the daemon
	cd $(WEB) && $(NPM) run dev

web-build: ## Build the open console into web/dist, which MAIL_WEB_DIR serves on a self-hosted server
	cd $(WEB) && $(NPM) run build

web-test: ## Run the open console's unit tests
	cd $(WEB) && $(NPM) test

web-check: ## Typecheck, test and build the open console
	cd $(WEB) && $(NPM) run typecheck && $(NPM) test && $(NPM) run build

clean: ## Remove build output
	rm -rf $(BIN) dist coverage.out
