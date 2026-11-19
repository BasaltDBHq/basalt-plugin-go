MODULE := github.com/BasaltDBHq/basalt-plugin-go
GOLANGCI_VERSION := v2.15.0
BIN := $(CURDIR)/bin
export PATH := $(BIN):$(PATH)

.PHONY: ci
ci: vet lint test ## What the pull request workflow runs

.PHONY: vet
vet:
	@go vet ./...

.PHONY: test
test: ## Tests with the race detector
	@go test -race -count=1 ./...

.PHONY: fmt
fmt:
	@go fmt ./...
	@go run mvdan.cc/gofumpt@v0.9.2 -l -w .
	@go run golang.org/x/tools/cmd/goimports@v0.39.0 -local $(MODULE) -w .

.PHONY: lint
lint: $(BIN)/golangci-lint
	@$(BIN)/golangci-lint config verify -c .golangci.yml
	@$(BIN)/golangci-lint run -c .golangci.yml ./...

$(BIN)/golangci-lint:
	@GOBIN=$(BIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

# The protobuf plugins come at the versions go.mod requires, so the generated
# code follows the module's own protobuf and connect versions.
$(BIN)/protoc-gen-go: go.mod
	@GOBIN=$(BIN) go install google.golang.org/protobuf/cmd/protoc-gen-go

$(BIN)/protoc-gen-connect-go: go.mod
	@GOBIN=$(BIN) go install connectrpc.com/connect/cmd/protoc-gen-connect-go

.PHONY: proto
proto: $(BIN)/protoc-gen-go $(BIN)/protoc-gen-connect-go ## Regenerate the Go code from proto/ (needs protoc on PATH)
	@protoc -I proto \
		--go_out=proto --go_opt=paths=source_relative \
		--connect-go_out=proto --connect-go_opt=paths=source_relative \
		proto/basalt/plugin/v1/plugin.proto
	@go run mvdan.cc/gofumpt@v0.9.2 -w proto
	@go run golang.org/x/tools/cmd/goimports@v0.39.0 -local $(MODULE) -w proto
