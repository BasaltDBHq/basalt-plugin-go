package basaltplugin

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/BasaltDBHq/basalt-plugin-go/protocol"
)

// SocketEnv is the one environment variable a local socket plugin is started
// with: the absolute path of the Unix socket it listens on. It is the engine's
// documented socket handover, repeated here so the SDK imports nothing of the
// engine; the engine's tests keep it equal to the engine's constant.
const SocketEnv = "BASALT_PLUGIN_SOCKET"

// Protocol is the protocol version this SDK speaks.
const Protocol = protocol.Version1

// Limits every connection runs under, from the protocol.
const (
	// MaxMessageBytes is the largest message either side may send.
	MaxMessageBytes = protocol.MaxMessageBytes
	// MaxInFlight is the number of hook calls the engine keeps open on a
	// plugin. health and shutdown are not counted: each has a slot of its own.
	MaxInFlight = protocol.MaxInFlight
)

// HookDecl is what a plugin declares for one hook: the same object as one
// entry of the manifest's hooks. It is the protocol package's type, the one
// the engine checks a declaration against.
type HookDecl = protocol.HookDecl

// Model names the model behind a plugin, when it has one. The engine records
// the name and version in the ledger.
type Model = protocol.Model

// Info is what a plugin tells the engine about itself. Name, Version and the
// hooks it declares must equal the installed manifest exactly, or the engine
// refuses the plugin.
type Info struct {
	Name    string
	Version string
	// Model is optional.
	Model *Model
}

// Plugin is the one interface every plugin implements.
//
// It is deliberately small. Everything else is an optional interface the SDK
// looks for with a type assertion: the lifecycle ones below (Initializer,
// HealthChecker, Shutdowner) and, as each hook's messages land, one interface
// per hook. A plugin implements only what it needs, and a new hook never adds a
// method to Plugin, so a plugin written today still compiles and runs when the
// SDK learns a new hook.
type Plugin interface {
	Info() Info
}

// Init is what the engine sends in initialize.
type Init struct {
	// Protocol is the version negotiated for this connection.
	Protocol string
	// Hooks are the hooks the engine will call: the manifest's.
	Hooks []string
	// Config is the entry's config from engine.json, {} when it has none. It
	// is never a secret.
	Config json.RawMessage
	// TimeoutMs is the deadline of every call, in milliseconds. The SDK
	// already applies it to the context each method receives.
	TimeoutMs int
}

// Initializer is implemented by a plugin that needs its config, or has to load
// something before it can answer. Returning an error refuses the connection;
// the error's text is not sent to the engine (it may hold a value), so a plugin
// that wants a reason writes it to its own stderr.
type Initializer interface {
	Initialize(ctx context.Context, in Init) error
}

// HealthChecker is implemented by a plugin that can be unready. A nil error
// answers READY, an error answers NOT_READY. A plugin without it is always
// READY once it runs.
type HealthChecker interface {
	Health(ctx context.Context) error
}

// Shutdowner is implemented by a plugin that must flush or release something
// before it exits. It runs when the engine sends shutdown, before the reply.
// Serve returns its error (after the reply has been sent).
type Shutdowner interface {
	Shutdown(ctx context.Context) error
}

// Errors Serve returns.
var (
	// ErrBadPlugin: the plugin's Info has no name or no version.
	ErrBadPlugin = errors.New("sdk: the plugin needs a name and a version")
	// ErrNoSocket: a socket transport was chosen and SocketEnv is not set.
	ErrNoSocket = errors.New("sdk: " + SocketEnv + " is not set to an absolute socket path")
	// ErrTLSRequired: a ConnectRPC listener that is not a Unix socket was
	// given without a TLS configuration. The engine always uses TLS for a
	// remote plugin.
	ErrTLSRequired = errors.New("sdk: a listener that is not a unix socket needs a TLS configuration")
	// ErrMessageTooLarge: the engine sent a message over MaxMessageBytes on
	// the stdio transport. The stream cannot be resynchronised, so Serve ends.
	ErrMessageTooLarge = errors.New("sdk: a message was over the size cap")
	// ErrHookNotImplemented: WithHooks declares a hook the SDK has an
	// interface for (FeedObserver, ApprovalDeliverer), and the plugin does
	// not implement it. The engine would call the hook and get "method not
	// found" every time. Implement the interface; the SDK then declares the
	// hook itself.
	ErrHookNotImplemented = errors.New("sdk: WithHooks declares a hook whose interface the plugin does not implement")
	// ErrTransport: the options name a transport that cannot be served as
	// asked.
	ErrTransport = errors.New("sdk: the transport options do not fit together")
)
