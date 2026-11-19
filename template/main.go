// Command template is the smallest plugin that passes `basalt plugin check`:
// a feed observer that writes one line per event to stderr. Copy this file,
// change the name and version, and make ObserveFeed do what your plugin does.
//
// It implements basaltplugin.FeedObserver, so the SDK declares feed.observe for it.
// To also deliver approval notices, implement basaltplugin.ApprovalDeliverer and add
// "approval.deliver": { "contract": "deliver/1" } to the manifest's hooks.
//
// The manifest that goes with it (basalt-plugin.json, next to the binary):
//
//	{
//	  "manifestVersion": 1, "name": "hello-feed", "version": "0.1.0", "publisher": "You",
//	  "protocol": ["basalt-plugin/1"],
//	  "hooks": { "feed.observe": { "contract": "feed/1" } },
//	  "endpoint": { "local": { "transport": "jsonrpc-stdio", "command": "bin/hello-feed" } },
//	  "files": { "bin/hello-feed": "sha256:<hash of the binary>" },
//	  "network": { "egress": [] }
//	}
//
// To serve another transport, change endpoint.local.transport to jsonrpc-unix
// or connectrpc and add "args": ["-transport", "<the same name>"]. The binary
// reads that flag; the engine starts it with the arguments the manifest names
// and with SocketEnv set for a socket transport.
//
// Write nothing to stdout: with jsonrpc-stdio it is the protocol's stream.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	basaltplugin "github.com/basaltdbhq/basalt-plugin-go"
)

type plugin struct{}

func (plugin) Info() basaltplugin.Info {
	return basaltplugin.Info{Name: "hello-feed", Version: "0.1.0"}
}

// ObserveFeed writes one line per event and says it handled them all. If a
// line cannot be written, it returns an error, and the engine sends the same
// events again later. Events can arrive twice: a real plugin dedups by
// ev.Source and ev.ID.
func (plugin) ObserveFeed(_ context.Context, batch basaltplugin.FeedBatch) (int64, error) {
	for _, ev := range batch.Events {
		if _, err := fmt.Fprintf(os.Stderr, "hello-feed: %s seq %d %s\n", batch.Agent, ev.Seq, ev.Type); err != nil {
			return 0, err
		}
	}
	return batch.LastSeq(), nil
}

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	fs := flag.NewFlagSet("hello-feed", flag.ContinueOnError)
	transport := fs.String("transport", "", "jsonrpc-stdio, jsonrpc-unix or connectrpc (default: from the environment)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var opts []basaltplugin.Option
	if *transport != "" {
		opts = append(opts, basaltplugin.WithTransport(basaltplugin.Transport(*transport)))
	}
	if err := basaltplugin.Serve(ctx, plugin{}, opts...); err != nil {
		fmt.Fprintln(os.Stderr, "hello-feed:", err)
		return 1
	}
	return 0
}
