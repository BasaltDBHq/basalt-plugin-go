package basaltplugin

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	pluginv1 "github.com/basaltdbhq/basalt-plugin-go/proto/basalt/plugin/v1"
	"github.com/basaltdbhq/basalt-plugin-go/proto/basalt/plugin/v1/pluginv1connect"
)

// TestStuckHandlersEndAtTheDeadlineOverConnect: the same bound holds on the
// ConnectRPC transport, and a stuck Shutdown still ends Serve.
func TestStuckHandlersEndAtTheDeadlineOverConnect(t *testing.T) {
	p := newStuck(t)
	s, path := serveSocket(t, p, WithTransport(ConnectRPC))
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	hc := &http.Client{Transport: &http.Transport{
		Protocols: &protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
	t.Cleanup(hc.CloseIdleConnections)
	cl := pluginv1connect.NewPluginServiceClient(hc, "http://plugin", connectrpc.WithGRPC())
	initReq := func() *connectrpc.Request[pluginv1.InitializeRequest] {
		return connectrpc.NewRequest(&pluginv1.InitializeRequest{
			Protocols: []string{"basalt-plugin/1"}, Limits: &pluginv1.Limits{TimeoutMs: 100},
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()

	_, err := cl.Initialize(ctx, initReq())
	require.NoError(t, err)

	p.stuckInit.Store(true)
	start := time.Now()
	_, err = cl.Initialize(ctx, initReq())
	require.Error(t, err)
	require.Less(t, time.Since(start), 2*time.Second)

	p.stuckHealth.Store(true)
	start = time.Now()
	h, err := cl.Health(ctx, connectrpc.NewRequest(&pluginv1.HealthRequest{}))
	require.NoError(t, err)
	require.Equal(t, "NOT_READY", h.Msg.GetStatus())
	require.Less(t, time.Since(start), 2*time.Second)

	p.stuckShutdown.Store(true)
	_, err = cl.Shutdown(ctx, connectrpc.NewRequest(&pluginv1.ShutdownRequest{}))
	require.NoError(t, err)
	require.NoError(t, s.await(t))
}

// stuckPlugin has handlers that can be made to ignore their context and never
// return, until release is closed.
type stuckPlugin struct {
	fullPlugin
	stuckInit, stuckHealth, stuckShutdown atomic.Bool
	release                               chan struct{}
	once                                  sync.Once
}

func newStuck(t *testing.T) *stuckPlugin {
	p := &stuckPlugin{fullPlugin: *newFull(), release: make(chan struct{})}
	t.Cleanup(p.free)
	return p
}

func (p *stuckPlugin) free() { p.once.Do(func() { close(p.release) }) }

func (p *stuckPlugin) Initialize(ctx context.Context, in Init) error {
	if p.stuckInit.Load() {
		<-p.release
	}
	return p.fullPlugin.Initialize(ctx, in)
}

func (p *stuckPlugin) Health(ctx context.Context) error {
	if p.stuckHealth.Load() {
		<-p.release
	}
	return p.fullPlugin.Health(ctx)
}

func (p *stuckPlugin) Shutdown(ctx context.Context) error {
	if p.stuckShutdown.Load() {
		<-p.release
	}
	return p.fullPlugin.Shutdown(ctx)
}

const shortInit = `{"protocols":["basalt-plugin/1"],"hooks":[],"config":{},"limits":{"timeoutMs":100}}`

// TestStuckHandlersDoNotHoldThePlatformMethods: a handler that never returns
// ends at the call's deadline instead of holding the call.
func TestStuckHandlersDoNotHoldThePlatformMethods(t *testing.T) {
	p := newStuck(t)
	s, c := serveStdio(t, p)
	require.Nil(t, c.call("initialize", shortInit).Error)

	p.stuckInit.Store(true)
	start := time.Now()
	r := c.call("initialize", shortInit)
	require.NotNil(t, r.Error)
	require.Equal(t, codeDeadline, r.Error.Code)
	require.Less(t, time.Since(start), 2*time.Second)

	p.stuckHealth.Store(true)
	start = time.Now()
	r = c.call("health", `{}`)
	require.Nil(t, r.Error)
	require.JSONEq(t, `{"status":"NOT_READY"}`, string(r.Result))
	require.Less(t, time.Since(start), 2*time.Second)

	p.stuckShutdown.Store(true)
	start = time.Now()
	r = c.call("shutdown", `{}`)
	require.Nil(t, r.Error, "shutdown answers even when the plugin's own does not return")
	require.Less(t, time.Since(start), 2*time.Second)
	require.NoError(t, s.await(t), "and Serve ends")
}

// TestCallsInFlightAtEOFAreAnswered: the engine may send a call, close stdin
// and then read the reply; the handler keeps a live context until it returns.
func TestCallsInFlightAtEOFAreAnswered(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var ctxErr atomic.Pointer[error]
	extra := map[string]methodFunc{"slow": func(ctx context.Context, _ json.RawMessage) (any, *rpcError) {
		close(started)
		<-release
		err := ctx.Err()
		ctxErr.Store(&err)
		return struct{}{}, nil
	}}
	s, c := serveStdio(t, newFull(), withExtra(extra))
	c.send(`{"jsonrpc":"2.0","id":1,"method":"slow","params":{}}`)
	<-started
	require.NoError(t, c.w.Close())

	select {
	case err := <-s.done:
		t.Fatalf("Serve returned (%v) with a call in flight", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	r := c.read()
	require.Nil(t, r.Error)
	require.JSONEq(t, `{}`, string(r.Result))
	require.NoError(t, s.await(t))
	require.NoError(t, *ctxErr.Load(), "the handler was not cancelled")
}

// TestCallsInFlightAtEOFEndAtTheirDeadline: a call that never finishes is
// cancelled by its deadline, answered, and does not hold Serve.
func TestCallsInFlightAtEOFEndAtTheirDeadline(t *testing.T) {
	extra := map[string]methodFunc{"slow": func(ctx context.Context, _ json.RawMessage) (any, *rpcError) {
		<-ctx.Done()
		return nil, nil
	}}
	s, c := serveStdio(t, newFull(), withExtra(extra))
	require.Nil(t, c.call("initialize", shortInit).Error)
	c.send(`{"jsonrpc":"2.0","id":9,"method":"slow","params":{}}`)
	require.NoError(t, c.w.Close())
	r := c.read()
	require.NotNil(t, r.Error)
	require.Equal(t, codeDeadline, r.Error.Code)
	require.NoError(t, s.await(t))
}
