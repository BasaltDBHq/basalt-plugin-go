package basaltplugin

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
)

// Serve runs the plugin until the engine sends shutdown, the engine closes the
// connection (stdio), or ctx ends, and then returns. It answers initialize,
// health and shutdown itself, enforces the 4 MiB message cap in both
// directions and the per-call deadline, and starts no work beyond what the
// engine asked for. A plugin author writes no protocol code.
//
// Serve returns nil for a clean end: a shutdown, a closed stdin, a cancelled
// ctx. It returns the plugin's own Shutdown error if there was one, and an
// error when it could not start serving (a missing socket variable, a socket
// that cannot be created) or, on stdio, when the engine sent a message over
// the cap. A plugin's main normally ends with:
//
//	if err := sdk.Serve(ctx, p, opts...); err != nil {
//		fmt.Fprintln(os.Stderr, err)
//		os.Exit(1)
//	}
//
// Serve reads one environment variable, SocketEnv, and only when it serves a
// socket. It never logs, and no error it returns or sends carries a value from
// a message.
func Serve(ctx context.Context, p Plugin, opts ...Option) error {
	cfg := newConfig(opts)
	s, err := newServer(p, cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.stop = cancel

	err = s.run(ctx, cfg)
	if err != nil {
		return err
	}
	return s.shutdownError()
}

func (s *server) run(ctx context.Context, cfg *config) error {
	t, err := cfg.resolve()
	if err != nil {
		return err
	}
	switch t {
	case JSONRPCStdio:
		return s.serveConn(ctx, cfg.stdin, cfg.stdout)
	case JSONRPCUnix:
		l, err := listenUnix(cfg)
		if err != nil {
			return err
		}
		return s.serveJSONRPCListener(ctx, l)
	case ConnectRPC:
		l := cfg.listener
		if l == nil {
			if l, err = listenUnix(cfg); err != nil {
				return err
			}
		}
		return s.serveConnect(ctx, l, cfg.tls, cfg.token)
	}
	return ErrTransport
}

// resolve picks the transport and checks the options agree with it.
func (c *config) resolve() (Transport, error) {
	t := c.transport
	if c.listener != nil {
		if t != "" && t != ConnectRPC {
			return "", ErrTransport
		}
		if _, isUnix := c.listener.Addr().(*net.UnixAddr); !isUnix && c.tls == nil {
			return "", ErrTLSRequired
		}
		return ConnectRPC, nil
	}
	if t == "" {
		t = JSONRPCStdio
		if c.getenv(SocketEnv) != "" {
			t = JSONRPCUnix
		}
	}
	switch t {
	case JSONRPCStdio, JSONRPCUnix, ConnectRPC:
		if c.token != "" && t != ConnectRPC {
			return "", ErrTransport
		}
		return t, nil
	}
	return "", ErrTransport
}

// listenUnix listens on the socket the engine handed over. The engine (or,
// in service mode, the plugin's unit) made the socket's directory reachable
// only by the right users, so the socket is created as the process's umask
// gives it. Closing the listener removes it. A service unit's directory
// stays between starts, so a socket a crash left at the path is removed
// first; anything else there is left and refused.
func listenUnix(c *config) (net.Listener, error) {
	path := c.getenv(SocketEnv)
	if path == "" || !filepath.IsAbs(path) {
		return nil, ErrNoSocket
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode().Type() == os.ModeSocket {
		_ = os.Remove(path)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, errors.New("basaltplugin: cannot listen on the plugin socket")
	}
	return l, nil
}
