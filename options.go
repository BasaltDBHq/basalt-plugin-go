package basaltplugin

import (
	"crypto/tls"
	"io"
	"net"
	"os"
)

// Transport is a way to carry the protocol. The values are the transport names
// a manifest's endpoint uses.
type Transport string

// The transports Serve can speak.
const (
	// JSONRPCStdio: newline-delimited JSON-RPC 2.0 on stdin and stdout. The
	// plugin must write nothing else to stdout.
	JSONRPCStdio Transport = "jsonrpc-stdio"
	// JSONRPCUnix: the same messages on a Unix socket at the path in SocketEnv.
	JSONRPCUnix Transport = "jsonrpc-unix"
	// ConnectRPC: the basalt.plugin.v1.PluginService service, served as
	// ConnectRPC and gRPC over HTTP/2. On a Unix socket at the path in
	// SocketEnv it is plain h2c, for a local plugin. On a listener given with
	// WithListener it is a remote plugin, over TLS.
	ConnectRPC Transport = "connectrpc"
)

// Option changes how Serve runs.
type Option func(*config)

type config struct {
	transport Transport
	hooks     map[string]HookDecl
	hooksSet  bool
	listener  net.Listener
	tls       *tls.Config
	token     string

	// Test seams, never exported.
	getenv func(string) string
	stdin  io.ReadCloser
	stdout io.Writer
	extra  map[string]methodFunc
}

func newConfig(opts []Option) *config {
	c := &config{getenv: os.Getenv, stdin: os.Stdin, stdout: os.Stdout}
	for _, o := range opts {
		o(c)
	}
	return c
}

// WithTransport chooses the transport. Without it, Serve speaks JSON-RPC on a
// Unix socket when SocketEnv is set and JSON-RPC on stdio when it is not, which
// is how the engine starts a plugin whose manifest says jsonrpc-stdio or
// jsonrpc-unix. A plugin whose manifest says connectrpc passes ConnectRPC.
//
// The simplest way to cover every manifest with one binary is to read the
// transport from the command's arguments, which the manifest sets, as the
// template does.
func WithTransport(t Transport) Option {
	return func(c *config) { c.transport = t }
}

// WithHooks declares exactly these hooks, replacing what the SDK finds from the
// interfaces the plugin implements. Use it to declare a hook whose SDK
// interface does not exist yet (the engine still checks the declaration against
// the manifest), or to declare fewer hooks than the plugin implements. The
// declaration must equal the manifest's hooks. A hook that has an SDK
// interface (FeedObserver, ApprovalDeliverer) can be declared only by a
// plugin that implements it; Serve returns ErrHookNotImplemented otherwise.
func WithHooks(h map[string]HookDecl) Option {
	return func(c *config) {
		c.hooks = h
		c.hooksSet = true
	}
}

// WithListener serves ConnectRPC on a listener the plugin supplies, for a
// plugin that runs on another host (or any plugin that picks its own socket).
// tlsConfig carries the plugin's certificate and is required unless l is a Unix
// socket: the engine refuses a remote endpoint without TLS and checks the
// certificate against the URL's host. Serve clones the configuration, sets a
// TLS 1.2 minimum when it has none, and offers HTTP/2. WithListener implies
// ConnectRPC; combining it with another transport is an error.
func WithListener(l net.Listener, tlsConfig *tls.Config) Option {
	return func(c *config) {
		c.listener = l
		c.tls = tlsConfig
	}
}

// WithBearerToken makes the ConnectRPC server refuse any call that does not
// carry "Authorization: Bearer <token>", the header the engine sends when the
// entry has an auth.credentialFile. The plugin supplies the token itself; the
// SDK never reads one from the environment. Use it with a remote listener.
func WithBearerToken(token string) Option {
	return func(c *config) { c.token = token }
}

// withExtra adds methods the plugin protocol does not have yet. Tests use it
// to hold the size cap, the deadline and the call slots to their limits.
func withExtra(m map[string]methodFunc) Option {
	return func(c *config) { c.extra = m }
}

func withEnv(f func(string) string) Option {
	return func(c *config) { c.getenv = f }
}

func withStdio(in io.ReadCloser, out io.Writer) Option {
	return func(c *config) {
		c.stdin = in
		c.stdout = out
	}
}
