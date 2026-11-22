// Package basaltplugin serves a Basalt plugin. A plugin author writes what the plugin
// does; the SDK speaks the protocol.
//
// The SDK answers initialize (with version negotiation), health and shutdown,
// enforces the 4 MiB message cap in both directions, applies the entry's
// per-call deadline to the context of every call, keeps at most four hook
// calls running at once (health and shutdown are answered beside them), and
// exits cleanly on shutdown. It never logs, never reads the environment
// beyond SocketEnv, and puts no value from a message in any error.
//
// # Write a notifier
//
// A notifier tells a person what an agent did, in the channel they already
// use. It implements one or both of two interfaces, and the SDK declares the
// matching hooks for it:
//
//   - FeedObserver (feed.observe): the agent's ledger events, in order, as
//     typed values. Metadata only, never a row value, a statement or an
//     intent, so they are safe to store or forward.
//   - ApprovalDeliverer (approval.deliver): a notice that a write waits for
//     the person's approval, a revert waits for their signature, or the link
//     to one was replaced, with the link the agent was handed.
//
// A complete notifier (chat stands for your channel's client). The package's
// name is basaltplugin, so import it by that name:
//
//	import basaltplugin "github.com/basaltdbhq/basalt-plugin-go"
//
//	type notifier struct{ out *chat.Client }
//
//	func (notifier) Info() basaltplugin.Info { return basaltplugin.Info{Name: "chat-notify", Version: "0.1.0"} }
//
//	func (n notifier) ObserveFeed(ctx context.Context, b basaltplugin.FeedBatch) (int64, error) {
//		for _, ev := range b.Events {
//			held, ok := ev.Detail.(basaltplugin.WriteHeld)
//			if !ok || ev.IsCheckSample() {
//				continue // another type, one this SDK does not know, or check's sample
//			}
//			text := fmt.Sprintf("%s: %d rows held", chat.Escape(b.Agent), held.Rows)
//			if err := n.out.Post(ctx, ev.Source+"/"+ev.ID, text); err != nil {
//				return 0, err // the engine sends this batch again later
//			}
//		}
//		return b.LastSeq(), nil
//	}
//
//	func (n notifier) DeliverApproval(ctx context.Context, x basaltplugin.Notice) error {
//		switch x.Kind {
//		case basaltplugin.KindApprovalRequested, basaltplugin.KindRevertRequested, basaltplugin.KindLinkReplaced:
//		default:
//			return nil // a kind this SDK does not know: skip it
//		}
//		if x.IsCheckSample() || time.Now().After(x.ExpiresAt) {
//			return nil
//		}
//		text := chat.Escape(x.Headline) + "\n" + x.Instruction
//		if x.Link != "" {
//			text += "\n" + x.Link
//			if x.LinkOpens == basaltplugin.LinkOpensEngineComputer {
//				text += "\n(This link opens only on the computer Basalt runs on.)"
//			}
//		}
//		return n.out.Post(ctx, x.Source+"/"+x.ID, text)
//	}
//
//	func main() { /* basaltplugin.Serve(ctx, notifier{...}), as the template does */ }
//
// Its manifest declares both hooks, with their contracts. The SDK declares a
// hook for every interface the plugin implements, so the manifest must list
// exactly those: implement ApprovalDeliverer without listing approval.deliver
// and the engine refuses the plugin at initialize.
//
//	"hooks": { "feed.observe": { "contract": "feed/1" }, "approval.deliver": { "contract": "deliver/1" } }
//
// What to know:
//
//   - At least once. An event or a notice can arrive again: after a crash, a
//     restart or a failed call, and a notice up to four times. Two with the
//     same Source and ID are the same: dedup by that pair, as the example does
//     by posting with Source and ID as the message key.
//   - The answer to a batch. Return the Seq of the last event you handled:
//     b.LastSeq() for all of them, or an earlier one, and the engine sends the
//     rest again. The SDK refuses to send a Seq that is not one of the batch's;
//     an error, a panic, a late answer or a refused one makes the engine send
//     the same events again later. Nothing in Basalt waits for a notifier.
//   - Skip what you do not know. A later engine may send a Type this SDK does
//     not know (Detail is nil, Seq is set) or a field it does not know
//     (ignored). Skip the event and still count it as handled; skip a notice
//     of an unknown Kind.
//   - Forward unchanged. Every event is a CloudEvents 1.0 event. Raw returns
//     its exact JSON bytes, so a plugin that passes events to a message broker
//     or a webhook sends Raw, never a JSON encoding of FeedEvent or Notice.
//   - Plain text. Headline, Table, Agent and every other string are the
//     engine's or the database's text, and an agent names its own tables. The
//     engine keeps them to one plain line, but a chat's own markup (mentions
//     such as @everyone or <!channel>, emphasis, links) is yours to escape
//     before you post. A notice's link opens anywhere (LinkOpensAnywhere) or
//     only on the computer Basalt runs on (LinkOpensEngineComputer): say so
//     when you show it. With no link, Instruction tells the person to ask the
//     agent.
//   - Samples. basalt plugin check sends a fixed batch and notice for which
//     IsCheckSample is true. Answer them as any other, but do not post them
//     to a person or keep them for dedup.
//   - What you get is what the engine sends. The SDK passes events and
//     notices on as they arrive; it is the engine that keeps row values,
//     statements, intents and other agents out of them.
//
// # What a notice's link holds
//
// A notice carries exactly the link the agent's own answer handed over. A
// short link carries only a secret that opens the approval page. A long link
// carries the whole request inside it: the statement, its parameters, the
// intent and the affected rows, values included. So anyone who can read
// where you deliver a notice can read the request, and a plugin that
// forwards notices to a message broker puts that link into the broker's log,
// readable by whoever can read that log for as long as it keeps it. The link
// grants no power over the action: approving still needs the person's
// passkey. Every other field of a notice, and every feed event, holds no
// value, statement or intent.
//
// # From zero to a served plugin
//
// The template (directory template, package main) is a feed observer that
// writes one line per event to stderr, with a transport flag and the manifest
// it needs. Copy template/main.go into a module of your own that requires
// this one (go get github.com/basaltdbhq/basalt-plugin-go). The steps use
// the basalt-plugin command (go install
// github.com/basaltdbhq/basalt-plugin-go/cmd/basalt-plugin@latest), which
// needs no engine:
//
//  1. Build it, from your module:
//     go build -o hello-feed/bin/hello-feed .
//  2. Write hello-feed/basalt-plugin.json. The template's comment has a
//     manifest to start from. Its name, version and hooks must equal what
//     Info and the interfaces the plugin implements say, or the engine
//     refuses the plugin at initialize.
//  3. Fill in the file hashes: basalt-plugin manifest hello-feed. Run it
//     again after every rebuild.
//  4. Check it: basalt-plugin check hello-feed. It starts the plugin the way
//     the engine does, runs initialize and health, sends each declared
//     notifier hook a fixed sample, and sends shutdown. It prints one line
//     for initialize and health and one per hook:
//     "initialize 3ms, health READY 1ms"
//     "feed.observe lastSeq 2 1ms"
//  5. To publish it, sign it: basalt-plugin keygen --out KEYDIR, then
//     basalt-plugin sign hello-feed --key KEYDIR/KEYID.key.
//
// A person installs a plugin through the engine, which asks them to approve
// it.
//
// # Choosing a transport
//
// The author writes no transport code. The manifest's endpoint.local.transport
// says how the engine starts and reaches the plugin, and Serve follows:
//
//   - jsonrpc-stdio: the default when SocketEnv is not set. The engine speaks
//     JSON-RPC on the plugin's stdin and stdout. Write nothing else to stdout.
//   - jsonrpc-unix: the default when SocketEnv is set. The engine starts the
//     plugin with that one environment variable, the absolute path of a socket;
//     Serve listens on it.
//   - connectrpc: pass WithTransport(ConnectRPC). With SocketEnv set, Serve
//     serves ConnectRPC and gRPC over h2c on that socket, so a plain gRPC
//     client works too.
//
// One binary can serve all three: read a flag, put it in the manifest's "args",
// and pass basaltplugin.WithTransport(basaltplugin.Transport(flagValue)), as
// the template does.
//
// A remote plugin (the manifest's endpoint.remote) serves ConnectRPC on a
// listener the author supplies, over TLS:
//
//	l, err := tls.Listen("tcp", ":8443", &tls.Config{Certificates: certs})
//	...
//	basaltplugin.Serve(ctx, p, basaltplugin.WithListener(l, tlsConfig), basaltplugin.WithBearerToken(token))
//
// The engine requires TLS and checks the certificate against the endpoint's
// host. WithBearerToken makes the server require the "Authorization: Bearer"
// header the engine sends when the entry has a credential file.
//
// # Optional behavior
//
// Plugin has one method. The SDK looks for more with type assertions, so a
// plugin implements only what it needs:
//
//   - Initializer receives the entry's config and limits at initialize.
//   - HealthChecker reports NOT_READY when it returns an error.
//   - Shutdowner runs before the shutdown reply, to flush or release.
//   - FeedObserver and ApprovalDeliverer, above, serve the notifier hooks.
//
// Handlers receive a context that ends at the call's deadline and must return
// when it does. A hook handler that does not keeps its slot until it returns:
// a later hook call waits for a slot until its own deadline and is then
// answered deadline exceeded, and health and shutdown are still answered.
// An error a handler returns is not sent to the engine (it may hold a value):
// the engine sees a failed call, and a plugin that wants a reason writes it
// to its own stderr. A notifier handler that panics fails its call the same
// way, and the plugin goes on serving.
//
// # Hooks
//
// The SDK declares a hook in initialize only for a plugin that implements its
// interface, so adding a hook to the SDK never changes a plugin written
// before it. The notifier hooks have interfaces; wire.detect and
// classify.suggest do not yet. Until the interface for your hook exists,
// declare it with WithHooks. WithHooks refuses to declare a hook that has an
// interface the plugin does not implement (ErrHookNotImplemented). How a
// hook is added to the SDK is described on hookSpec.
package basaltplugin
