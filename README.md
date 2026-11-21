# basalt-plugin-go

The Go SDK for [BasaltDB](https://github.com/BasaltDBHq/BasaltDB) plugins.

BasaltDB sits between an AI agent and a database. A plugin is a separate program the engine starts and calls. You write what the plugin does; the SDK speaks the protocol: the handshake, health checks, shutdown, message size limits, deadlines and the three transports.

The first kind of plugin is a **notifier**. It tells a person what an agent did, in the channel they already use: a chat room, a pager, a webhook, a message queue.

Full documentation: <https://docs.basaltvault.app>

> The first release is `v0.1.0-alpha`. The API can still change before `v1.0.0`.

## Install

You need Go 1.26 or later.

```sh
go get github.com/basaltdbhq/basalt-plugin-go@v0.1.0-alpha
```

The module path is all lower case.

The package name is `basaltplugin`, so import it by that name:

```go
import basaltplugin "github.com/basaltdbhq/basalt-plugin-go"
```

## A complete notifier

This is the whole program. It is also in [`examples/notifier`](examples/notifier/main.go).

```go
package main

import (
	"context"
	"fmt"
	"os"

	basaltplugin "github.com/basaltdbhq/basalt-plugin-go"
)

type notifier struct{}

func (notifier) Info() basaltplugin.Info {
	return basaltplugin.Info{Name: "hello-notify", Version: "0.1.0"}
}

func (notifier) ObserveFeed(_ context.Context, b basaltplugin.FeedBatch) (int64, error) {
	for _, ev := range b.Events {
		held, ok := ev.Detail.(basaltplugin.WriteHeld)
		if !ok || ev.IsCheckSample() {
			continue
		}
		if _, err := fmt.Fprintf(os.Stderr, "%s: %d rows wait for approval\n", b.Agent, held.Rows); err != nil {
			return 0, err // the engine sends this batch again later
		}
	}
	return b.LastSeq(), nil
}

func (notifier) DeliverApproval(_ context.Context, n basaltplugin.Notice) error {
	if n.IsCheckSample() {
		return nil
	}
	_, err := fmt.Fprintln(os.Stderr, n.Headline, n.Instruction, n.Link)
	return err
}

func main() {
	if err := basaltplugin.Serve(context.Background(), notifier{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

What it does:

- `Info` names the plugin. The name and version must equal the manifest's.
- `ObserveFeed` gets one agent's events, in order: writes applied, refused or held, approvals signed, reverts. They hold metadata only, never a row value or a statement. Return the `Seq` of the last event you handled; `b.LastSeq()` means all of them. Return an error and the engine sends the same events again later.
- `DeliverApproval` gets a notice that something waits for the person: a held write, a prepared revert, or a replaced link.
- `Serve` runs until the engine stops the plugin.

The SDK declares a hook for each of the two interfaces the plugin implements. Implement only one and it declares only that one.

Write nothing to stdout. On the default transport, stdout carries the protocol. Use stderr for your own output.

## Check, hash and sign with `basalt-plugin`

`basalt-plugin` is the author's tool. It runs on your machine, as you, and needs no engine, no database, no signature and no trusted key.

```sh
go install github.com/basaltdbhq/basalt-plugin-go/cmd/basalt-plugin@latest
```

1. Build the plugin into a folder of its own:

   ```sh
   go build -o hello-notify/bin/hello-notify .
   ```

2. Write `hello-notify/basalt-plugin.json`, with no `files` entry. The name, the version and the hooks must equal what the plugin says about itself, or the engine refuses it.

   ```json
   {
     "manifestVersion": 1, "name": "hello-notify", "version": "0.1.0", "publisher": "You",
     "protocol": ["basalt-plugin/1"],
     "hooks": {
       "feed.observe": { "contract": "feed/1" },
       "approval.deliver": { "contract": "deliver/1" }
     },
     "endpoint": { "local": { "transport": "jsonrpc-stdio", "command": "bin/hello-notify" } },
     "network": { "egress": [] }
   }
   ```

3. Fill in the file hashes. Run it again after every rebuild:

   ```sh
   basalt-plugin manifest hello-notify
   ```

   It writes the manifest's `files` from what the folder holds: every file but the manifest and its signature, each with its SHA-256. No other byte of the manifest changes. It refuses what the engine refuses: a symlink, an empty directory, a file the manifest cannot name (a `.DS_Store`, for one).

4. Check it:

   ```sh
   basalt-plugin check hello-notify
   ```

   It starts your program the way the engine does (its own process group, the folder as the working directory, no environment but `BASALT_PLUGIN_SOCKET` for a socket transport), runs the handshake and a health check, sends each hook a fixed sample, and stops it. It prints one line for the handshake and one per hook:

   ```
   initialize 3ms, health READY 1ms
   approval.deliver 1ms
   feed.observe lastSeq 2 1ms
   ```

   It also refuses a folder the engine would refuse: a hash that is not the manifest's, a file the manifest does not list, a plugin that names another version or other hooks than its manifest. It does not run as root, and it has nothing to start for a remote plugin. All three transports work.

The samples `check` sends answer `true` to `IsCheckSample()`. Answer them like any other event, but do not post them to a person.

### Sign a plugin

A person who installs your plugin approves it on the engine's page. If you publish plugins for others, sign the folder so they can tell it came from you:

```sh
basalt-plugin keygen --out keys
basalt-plugin sign hello-notify --key keys/<key id>.key
```

`keygen` writes `keys/<key id>.key` (mode `0600`, never printed) and `keys/<key id>.pub`, and never overwrites a file. `sign` checks every listed file against its hash, then writes `basalt-plugin.json.sig` next to the manifest: ECDSA P-256 over the manifest's exact bytes. It refuses a key file that others can read, a symlink, or a file owned by someone else. Sign after the last `manifest`: a change to the manifest or to a listed file breaks the signature, and `manifest` removes a signature it has made stale. Keep the private key secret.

The same code is the package `plugintool`, for a build script that wants it without the command.

## Start from the template

[`template/main.go`](template/main.go) is a feed observer with a `-transport` flag and the manifest it needs in its comment. Copy the file into your own module, change the name and version, and make `ObserveFeed` do what your plugin does.

## What to know before you ship

- **Events arrive at least once.** After a crash, a restart or a failed call the engine sends them again. Two events with the same `Source` and `ID` are the same event: dedup by that pair.
- **Skip what you do not know.** A later engine may send an event type this SDK does not know (`Detail` is nil, `Seq` is set) or a notice of an unknown `Kind`. Skip it and still count it as handled.
- **Forward events unchanged.** Every event is a CloudEvents 1.0 event. `Raw()` returns its exact JSON bytes; send those to a broker or a webhook, not a re-encoding.
- **Escape text for your channel.** Headlines, table names and agent names are plain text. An agent names its own tables, so escape your chat's markup (mentions, links, emphasis) before you post.
- **A notice's link can hold the request.** A long link carries the statement, its parameters and the affected rows. Anyone who can read where you deliver the notice can read them. The link grants no power: approving still needs the person's passkey.
- **Errors stay in the plugin.** The SDK never sends a handler's error text to the engine, because it may hold a value. Write reasons to stderr.

The [package documentation](https://pkg.go.dev/github.com/basaltdbhq/basalt-plugin-go) covers each of these, the optional `Initializer`, `HealthChecker` and `Shutdowner` interfaces, and remote plugins over TLS.

## Transports

You write no transport code. The manifest's `endpoint.local.transport` says how the engine reaches the plugin, and `Serve` follows.

| Manifest transport | What to do |
| --- | --- |
| `jsonrpc-stdio` | Nothing. It is the default. |
| `jsonrpc-unix` | Nothing. The engine sets `BASALT_PLUGIN_SOCKET` and `Serve` listens on it. |
| `connectrpc` | Pass `basaltplugin.WithTransport(basaltplugin.ConnectRPC)`. A plain gRPC client works too. |

A plugin on another host serves ConnectRPC over TLS on a listener you supply, with `WithListener` and `WithBearerToken`.

## Packages

| Import path | What it is |
| --- | --- |
| `github.com/basaltdbhq/basalt-plugin-go` | Package `basaltplugin`: the SDK. Most plugins need only this. |
| `.../plugintool` | Key generation, manifest hashes, signing and folder check, the code behind `basalt-plugin`. |
| `.../protocol` | The protocol's JSON message types and limits, for code that speaks the protocol itself. |
| `.../cmd/basalt-plugin` | The author's command: `keygen`, `manifest`, `sign`, `check`. |
| `.../proto/basalt/plugin/v1` and `.../pluginv1connect` | The generated ConnectRPC service, `basalt.plugin.v1.PluginService`, and its `.proto` file. |

## Protocol and versions

The engine defines the plugin protocol: its contract, its JSON schemas and the service definition. This module holds copies of what it needs: the `.proto` file under `proto/` (it differs from the engine's only in its header comment and `go_package`), the message types in `protocol/`, and the contract's samples under `testdata/` (`plugintool/samples/` holds the two the check sends). The signature format and the manifest rules `basalt-plugin` follows are the engine's; `plugintool/testdata/sign-vector/` holds a plugin and signatures made by both sides, and the engine's tests verify the same files. They were taken from BasaltDB commit `d2fd2b9c340e`, which is not yet in a tagged engine release.

This module's major version follows the protocol's. Every `v0` and `v1` release speaks `basalt-plugin/1`; a new protocol major version means a new module major version. The event type names (`dev.basaltdb.*`) are the engine's and do not change with this module's version.

## Develop

```sh
make help    # every target
make fmt     # gofumpt and goimports; commit what it changes
make ci      # tidy check, vet, lint (golangci-lint pinned in ./bin) and the tests with the race detector
make proto   # regenerate the Go code from the .proto file (needs protoc)
```

Code rules for contributors and coding agents: [AGENTS.md](AGENTS.md).

## License

Apache-2.0. See [LICENSE](LICENSE).
