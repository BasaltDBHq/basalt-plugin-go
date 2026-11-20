# basalt-plugin-go — agent guide

The Go SDK for BasaltDB plugins. A plugin is a separate program the engine starts and calls; the plugin author writes what it does, and this SDK speaks the protocol: the handshake, health checks, shutdown, message size limits, deadlines and the three transports (`jsonrpc-stdio`, `jsonrpc-unix`, `connectrpc`). This is a public library: every exported name is API that plugin authors build on.

Read [README.md](README.md) for what the SDK promises plugin authors, and the package documentation in [doc.go](doc.go) for the details. A change that breaks one of those promises is a bug, whatever a test says.

**Source of truth:** the engine defines the plugin protocol (its contract, JSON schemas and service definition). This module holds copies: the `.proto` under `proto/`, the message types in `protocol/`, the contract's samples under `testdata/`. Change the protocol in the engine first, then copy it here and say which engine commit it came from in the README's "Protocol and versions" section. Never change a copy on its own.

## Repo quick facts

- Module: `github.com/basaltdbhq/basalt-plugin-go` (all lower case); package name `basaltplugin`.
- **Before every push:** `make fmt` and commit what it changes, then `make lint` (0 issues). CI fails on gofumpt and goimports drift.
- **CI locally:** `make help`, then `make ci` (tidy check, vet, lint, race tests). CI runs the same targets on Linux and macOS.
- **Commits and PR titles:** Conventional Commits `type(scope): message`, lowercase imperative, no trailing period.
- Never push to `main` directly.

## Layout

```
*.go                  package basaltplugin: Serve, options, the notifier interfaces, typed events
protocol/             the protocol's JSON message types and limits (copied from the engine)
proto/                plugin.proto and the generated ConnectRPC service (make proto)
examples/notifier     a complete notifier, the one the README shows
template/             a starting point plugin authors copy
testdata/             the contract's sample messages; tests decode every one
```

## Rules that shape code

- **The SDK never logs, never writes to stdout and never reads the environment** beyond `SocketEnv`. On the stdio transport, stdout is the protocol.
- **No value from a message reaches an error.** Errors the SDK returns or sends to the engine carry fixed text; a handler's error text is never sent to the engine, because it may hold a value.
- **Compatibility:** the module's major version follows the protocol's (`basalt-plugin/1` is every `v0` and `v1`). Do not remove or change an exported name without a deliberate release decision; add instead. Unknown JSON fields are ignored on decode, and an unknown event type or notice kind reaches the plugin (with `Detail` nil), never an error.
- **Events are CloudEvents 1.0.** `Raw()` returns the exact bytes received; never re-encode an event the author may forward.
- Generated code under `proto/` is never edited by hand: change the `.proto`, run `make proto`.
- The README example, `examples/notifier` and `template/` must compile and match each other when the API they use changes.

## Go code

- **No `else` by default.** Set the default first and override in an `if`, or return early. Prefer `switch` over if/else-if chains. Enforced by revive (`early-return`, `indent-error-flow`, `superfluous-else`).
- **Early return and flat control flow.** Guard clauses over nested `if`. Type asserts use `v, ok := x.(T)` and handle `!ok`; on errors use `errors.As` and `errors.Is` (errorlint).
- **DRY:** extract a small private helper when the same non-trivial logic appears twice or more.
- **Magic numbers:** name them (`mnd` is on), with a short comment saying where the value comes from (the protocol's limits, a default).
- No cgo. The SDK's dependencies stay few: think twice before adding a module a plugin author will then carry.

## Testing

- `testing` + `testify/require`; small in-repo fakes over generated mocks.
- `go.uber.org/goleak` checks the root package's tests for leaked goroutines (`main_test.go`): `Serve` must stop everything it started. A new test package that starts goroutines adds its own `TestMain` with goleak.
- Behavior that every transport shares is tested on each of them (`transports_test.go`); every sample under `testdata/` is decoded by a test.
- `-race` on every run (`make test`).

## Tooling

- **Makefile** is the entry point; `.misc/make/tools.Makefile` pins golangci-lint, gotestsum and the protobuf plugins into `./bin`, and the targets always use those copies. `make help` lists the targets.
- **golangci-lint v2** with `.golangci.yml`, kept in step with the BasaltDB engine's config. Fix every finding; `//nolint` only with the specific linter and an explanation (`nolintlint` enforces both).
- Do not commit `bin/`, `tmp/` or credentials.
