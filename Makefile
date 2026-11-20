export PATH := $(CURDIR)/bin:$(PATH)
export SHELL := bash

include ./.misc/make/tools.Makefile
include ./.misc/make/help.Makefile

.DEFAULT_GOAL := help

MODULE := github.com/basaltdbhq/basalt-plugin-go
PKG ?= ./...

# ~~~ Development ~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

.PHONY: deps
deps: bin/golangci-lint bin/gotestsum ## Install pinned dev tools into ./bin

.PHONY: fmt
fmt: ## Format with gofmt, gofumpt, goimports
	@go fmt ./...
	@go run mvdan.cc/gofumpt@$(GOFUMPT_VERSION) -l -w .
	@go run golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION) -local $(MODULE) -w .

.PHONY: lint
lint: bin/golangci-lint ## Run golangci-lint (includes the formatters check and a config check)
	@$(GOLANGCI) config verify -c .golangci.yml
	@$(GOLANGCI) run -c .golangci.yml ./...

.PHONY: vet
vet: ## go vet
	@go vet ./...

.PHONY: tidy-check
tidy-check: ## go.mod and go.sum are tidy
	@go mod tidy
	@git diff --exit-code -- go.mod go.sum

.PHONY: test
test: bin/gotestsum ## Race tests
	@$(GOTESTSUM) --format pkgname-and-test-fails --hide-summary=skipped -- -race -count=1 -timeout 10m $(PKG)

.PHONY: test-cover
test-cover: bin/gotestsum ## Race tests with coverage.out
	@$(GOTESTSUM) --format pkgname -- -race -count=1 -timeout 10m -coverprofile=coverage.out -covermode=atomic $(PKG)
	@go tool cover -func=coverage.out | tail -1

# ~~~ Generate ~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

.PHONY: proto
proto: bin/protoc-gen-go bin/protoc-gen-connect-go ## Regenerate the Go code from proto/ (needs protoc on PATH)
	@protoc -I proto \
		--go_out=proto --go_opt=paths=source_relative \
		--connect-go_out=proto --connect-go_opt=paths=source_relative \
		proto/basalt/plugin/v1/plugin.proto
	@go run mvdan.cc/gofumpt@$(GOFUMPT_VERSION) -w proto
	@go run golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION) -local $(MODULE) -w proto

# ~~~ CI ~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

.PHONY: ci
ci: tidy-check vet lint test ## What the pull request workflow runs
