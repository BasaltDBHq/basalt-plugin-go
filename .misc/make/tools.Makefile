# Pinned dev tools, installed into ./bin. The Makefile always runs these
# copies, never one found on PATH, so every machine lints with the same
# version CI does.

bin:
	@ mkdir -p $@

GOLANGCI := $(CURDIR)/bin/golangci-lint
GOTESTSUM := $(CURDIR)/bin/gotestsum

GOFUMPT_VERSION := v0.9.2
GOIMPORTS_VERSION := v0.39.0

bin/golangci-lint: VERSION := v2.15.0
bin/golangci-lint: | bin
	@ printf "Install golangci-lint... "
	@ GOBIN=$(CURDIR)/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(VERSION)
	@ echo "done."

bin/gotestsum: VERSION := v1.13.0
bin/gotestsum: | bin
	@ printf "Install gotestsum... "
	@ GOBIN=$(CURDIR)/bin go install gotest.tools/gotestsum@$(VERSION)
	@ echo "done."

# The protobuf plugins come at the versions go.mod requires, so the generated
# code follows the module's own protobuf and connect versions.
bin/protoc-gen-go: go.mod | bin
	@ GOBIN=$(CURDIR)/bin go install google.golang.org/protobuf/cmd/protoc-gen-go

bin/protoc-gen-connect-go: go.mod | bin
	@ GOBIN=$(CURDIR)/bin go install connectrpc.com/connect/cmd/protoc-gen-connect-go
