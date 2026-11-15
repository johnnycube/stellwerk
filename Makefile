# Task-runner contract (https://fleetlint.org/baseline/#task-runner-contract). `make check` is the definition of green.
SHELL := bash
.SHELLFLAGS := -euo pipefail -c
.ONESHELL:
.DEFAULT_GOAL := check-fast
MAIN_BRANCH ?= main
# goolm = pure Go Olm, no libolm. CGO stays on for SQLite.
TAGS        ?= goolm
export CGO_ENABLED := 1
# Total statement coverage the cover target enforces.
COVER_MIN   ?= 75
PKGS        := ./...
LICENSES    ?= MIT,Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,MPL-2.0
# Mutation testing scope and floor (weekly, not part of check).
MUTATE_PKG  ?= ./internal/core/domain
MUTATE_MIN  ?= 70
# Each pin carries a `# renovate:` comment so Renovate bumps it (customManagers in renovate.json).
# renovate: datasource=go depName=github.com/golangci/golangci-lint/v2
GOLANGCI_VERSION    ?= v2.15.0
# renovate: datasource=go depName=golang.org/x/vuln
GOVULNCHECK_VERSION ?= v1.9.0
# renovate: datasource=go depName=github.com/google/go-licenses/v2
GO_LICENSES_VERSION ?= v2.0.1
# renovate: datasource=go depName=github.com/go-gremlins/gremlins
GREMLINS_VERSION    ?= v0.6.0
# renovate: datasource=go depName=github.com/zricethezav/gitleaks/v8
GITLEAKS_VERSION    ?= v8.30.1
# renovate: datasource=go depName=github.com/fleetlint/fleetlint
FLEETLINT_VERSION   ?= v0.11.0
# Tools are built with this module's toolchain: golangci-lint refuses code that targets a newer Go than it was built with.
TOOLCHAIN   := $(shell go env GOVERSION)

.PHONY: help tools fmt lint vet test cover audit bench build dist check-fast check schema example mutate release tidy clean

help: ## list targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*##' '{printf "  %-12s %s\n", $$1, $$2}'

tools: ## install the pinned Go tools the gates use
	GOTOOLCHAIN=$(TOOLCHAIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	GOTOOLCHAIN=$(TOOLCHAIN) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	GOTOOLCHAIN=$(TOOLCHAIN) go install github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION)
	GOTOOLCHAIN=$(TOOLCHAIN) go install github.com/go-gremlins/gremlins/cmd/gremlins@$(GREMLINS_VERSION)
	go install github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION)
	go install github.com/fleetlint/fleetlint/cmd/fleetlint@$(FLEETLINT_VERSION)

fmt: ## format in place
	golangci-lint fmt

lint: ## format check + linters + vet, warnings as errors
	golangci-lint fmt --diff
	golangci-lint run
	go vet -tags "$(TAGS)" $(PKGS)
	go vet -tags "$(TAGS) noha" $(PKGS)

vet: ## go vet only
	go vet -tags "$(TAGS)" $(PKGS)

test: ## all tests with the race detector, shuffled, with and without HA, plus the kill and restart test
	go test -tags "$(TAGS)" -race -shuffle=on -count=1 $(PKGS)
	go test -tags "$(TAGS) noha" -count=1 $(PKGS)
	go test -tags "$(TAGS) process" -count=1 ./test/process

cover: ## tests with coverage; fail if the total is below COVER_MIN
	go test -tags "$(TAGS)" -count=1 -coverprofile=cover.out $(PKGS)
	total=$$(go tool cover -func=cover.out | awk '/^total:/ {sub("%", "", $$3); print $$3}')
	echo "coverage $$total% (minimum $(COVER_MIN)%)"
	if [ "$${total%.*}" -lt $(COVER_MIN) ]; then echo "coverage below $(COVER_MIN)%"; exit 1; fi

# go-licenses spots stdlib packages by GOROOT; pass the active toolchain's, not the one it was built with
audit: ## vulnerabilities, licenses, tidy module graph, secrets
	govulncheck -tags "$(TAGS)" $(PKGS)
	GOROOT=$$(go env GOROOT) go-licenses check -tags "$(TAGS)" $(PKGS) --allowed_licenses=$(LICENSES)
	go mod tidy -diff
	gitleaks git --no-banner --redact .

schema: ## the checked-in config schema matches the code
	go run -tags "$(TAGS)" ./cmd/stellwerk -schema | diff - docs/stellwerk.schema.json

example: build ## the example config is valid and its rule tests pass
	env TOKEN_ALERTMANAGER=a TOKEN_SONARR=b TOKEN_DEFAULT=c FORGEJO_WEBHOOK_SECRET=d \
	    TOKEN_BACKUP=e TOKEN_UPTIME=f TOKEN_PROXMOX=g TOKEN_BACKUP_PING=h \
	    sh -c './stellwerk -config stellwerk.example.yaml -check && ./stellwerk -config stellwerk.example.yaml -test'

bench: ## benchmarks (not part of check)
	go test -tags "$(TAGS)" -run='^$$' -bench=. -benchmem $(PKGS)

build: ## build the binary
	go build -tags "$(TAGS)" -trimpath -o stellwerk ./cmd/stellwerk

dist: ## a release archive for this machine into dist/ (the release workflow builds and signs the real ones)
	v=$$(git describe --tags --always --dirty)
	name="stellwerk_$${v#v}_$$(go env GOOS)_$$(go env GOARCH)"
	mkdir -p "dist/$$name"
	go build -tags "$(TAGS)" -trimpath -ldflags="-s -w -X main.version=$$v" -o "dist/$$name/stellwerk" ./cmd/stellwerk
	cp LICENSE NOTICE README.md CHANGELOG.md stellwerk.example.yaml "dist/$$name/"
	tar -C dist -czf "dist/$$name.tar.gz" "$$name"

mutate: ## mutation testing on the domain core, fail below MUTATE_MIN % efficacy (weekly in CI)
	gremlins unleash --tags "$(TAGS)" --threshold-efficacy $(MUTATE_MIN) $(MUTATE_PKG)

check-fast: ## before every commit (< 30 s)
	golangci-lint fmt --diff
	golangci-lint run --new-from-rev=origin/$(MAIN_BRANCH) || golangci-lint run
	go test -tags "$(TAGS)" -short $(PKGS)

check: lint test cover audit schema example ## definition of green (< 10 min)
	fleetlint check --fail-on error

release: ## refuse dirty/non-main -> check -> signed tag V (its CHANGELOG.md section must exist); pushing the tag runs the release workflow
	@test -n "$(V)" || { echo "usage: make release V=vX.Y.Z"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "dirty tree"; exit 1; }
	@test "$$(git branch --show-current)" = "$(MAIN_BRANCH)" || { echo "not on $(MAIN_BRANCH)"; exit 1; }
	@grep -q "^## \[$(V:v%=%)\] " CHANGELOG.md || { echo "CHANGELOG.md has no '## [$(V:v%=%)] ' section"; exit 1; }
	$(MAKE) check
	git tag -s "$(V)" -m "stellwerk $(V)"
	@echo "tagged $(V), push with: git push origin $(V)"

tidy: ## go mod tidy
	go mod tidy

clean: ## remove build and coverage output
	rm -rf stellwerk cover.out dist
