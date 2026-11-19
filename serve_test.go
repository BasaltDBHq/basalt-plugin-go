package basaltplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/basaltdbhq/basalt-plugin-go/protocol"
)

const marker = "SECRET-PLUGIN-VALUE"

// fullPlugin implements every optional interface and records what it saw.
type fullPlugin struct {
	info       Info
	initErr    error
	unhealthy  atomic.Bool
	shutdownIn atomic.Int32
	shutdownEr error
	gotInit    atomic.Pointer[Init]
}

func (p *fullPlugin) Info() Info { return p.info }

func (p *fullPlugin) Initialize(_ context.Context, in Init) error {
	p.gotInit.Store(&in)
	return p.initErr
}

func (p *fullPlugin) Health(context.Context) error {
	if p.unhealthy.Load() {
		return errors.New(marker)
	}
	return nil
}

func (p *fullPlugin) Shutdown(context.Context) error {
	p.shutdownIn.Add(1)
	return p.shutdownEr
}

func newFull() *fullPlugin {
	return &fullPlugin{info: Info{Name: "acme-feed", Version: "2.1.0", Model: &Model{Name: "tiny", Version: "3"}}}
}

var wireHook = map[string]HookDecl{"wire.detect": {Contract: "detector/1"}}

// The SDK's tests are not parallel: some swap the package's hook table.

func TestStdioLifecycle(t *testing.T) {
	p := newFull()
	s, c := serveStdio(t, p, WithHooks(wireHook))

	r := c.call("initialize", `{"protocols":["basalt-plugin/0","basalt-plugin/1"],"hooks":["wire.detect"],"config":{"k":[1,"x"]},"limits":{"maxMessageBytes":4194304,"maxInFlight":4,"timeoutMs":1500}}`)
	require.Nil(t, r.Error)
	var res protocol.InitializeResponse
	require.NoError(t, json.Unmarshal(r.Result, &res))
	require.Equal(t, protocol.InitializeResponse{
		Protocol: protocol.Version1, Name: "acme-feed", Version: "2.1.0",
		Hooks: map[string]protocol.HookDecl{"wire.detect": {Contract: "detector/1"}},
		Model: &protocol.Model{Name: "tiny", Version: "3"},
	}, res)
	in := p.gotInit.Load()
	require.NotNil(t, in)
	require.JSONEq(t, `{"k":[1,"x"]}`, string(in.Config))
	require.Equal(t, 1500, in.TimeoutMs)
	require.Equal(t, []string{"wire.detect"}, in.Hooks)

	r = c.call("health", `{}`)
	require.JSONEq(t, `{"status":"READY"}`, string(r.Result))
	p.unhealthy.Store(true)
	r = c.call("health", `{}`)
	require.JSONEq(t, `{"status":"NOT_READY"}`, string(r.Result))
	require.NotContains(t, string(r.Result), marker)

	r = c.call("shutdown", `{}`)
	require.Nil(t, r.Error)
	require.JSONEq(t, `{}`, string(r.Result))
	require.NoError(t, s.await(t), "shutdown ends Serve cleanly")
	require.EqualValues(t, 1, p.shutdownIn.Load())
}

func TestShutdownErrorIsReturnedAfterTheReply(t *testing.T) {
	p := newFull()
	p.shutdownEr = errors.New("flush failed")
	s, c := serveStdio(t, p)
	r := c.call("shutdown", `{}`)
	require.Nil(t, r.Error)
	require.ErrorIs(t, s.await(t), p.shutdownEr)
}

func TestStdinClosedEndsServe(t *testing.T) {
	s, c := serveStdio(t, newFull())
	require.NoError(t, c.w.Close())
	require.NoError(t, s.await(t))
}

func TestContextCancelStopsServing(t *testing.T) {
	t.Run("stdio", func(t *testing.T) {
		s, c := serveStdio(t, newFull())
		c.call("health", `{}`)
		require.NoError(t, s.stop(t))
	})
	t.Run("jsonrpc-unix", func(t *testing.T) {
		s, path := serveSocket(t, newFull())
		require.NoError(t, s.stop(t))
		_, err := os.Lstat(path)
		require.ErrorIs(t, err, os.ErrNotExist, "the socket is removed")
	})
	t.Run("connectrpc", func(t *testing.T) {
		s, path := serveSocket(t, newFull(), WithTransport(ConnectRPC))
		require.NoError(t, s.stop(t))
		_, err := os.Lstat(path)
		require.ErrorIs(t, err, os.ErrNotExist, "the socket is removed")
	})
}

func TestVersionNegotiation(t *testing.T) {
	_, c := serveStdio(t, newFull(), WithHooks(wireHook))
	for _, offered := range []string{`["basalt-plugin/2"]`, `["basalt-plugin/0"]`, `[]`, `["garbage"]`} {
		r := c.call("initialize", fmt.Sprintf(`{"protocols":%s,"hooks":["wire.detect"],"config":{},"limits":{"timeoutMs":1000}}`, offered))
		require.Nil(t, r.Result, offered)
		require.NotNil(t, r.Error, offered)
		require.Equal(t, codeRefused, r.Error.Code)
		require.Contains(t, r.Error.Message, "basalt-plugin/1")
	}
	// The connection is still usable, and a good offer still works.
	r := c.call("initialize", `{"protocols":["basalt-plugin/2","basalt-plugin/1"],"hooks":["wire.detect"],"config":{},"limits":{"timeoutMs":1000}}`)
	require.Nil(t, r.Error)
}

func TestInitializeRefusesAHookThePluginDoesNotImplement(t *testing.T) {
	_, c := serveStdio(t, newFull()) // no hooks declared
	r := c.call("initialize", `{"protocols":["basalt-plugin/1"],"hooks":["wire.detect"],"config":{},"limits":{"timeoutMs":1000}}`)
	require.NotNil(t, r.Error)
	require.Equal(t, codeRefused, r.Error.Code)

	// Declared hooks are exactly what WithHooks says, nothing else.
	_, c = serveStdio(t, newFull(), WithHooks(wireHook))
	r = c.call("initialize", `{"protocols":["basalt-plugin/1"],"hooks":["wire.detect"],"config":{},"limits":{"timeoutMs":1000}}`)
	require.Nil(t, r.Error)
	var res protocol.InitializeResponse
	require.NoError(t, json.Unmarshal(r.Result, &res))
	require.Len(t, res.Hooks, 1)
}

func TestInitializeKeepsAPluginsErrorTextAway(t *testing.T) {
	p := newFull()
	p.initErr = errors.New(marker)
	_, c := serveStdio(t, p)
	r := c.call("initialize", initParams)
	require.NotNil(t, r.Error)
	require.NotContains(t, r.Error.Message, marker)
}

func TestInitializeRejectsATimeoutOutsideTheContract(t *testing.T) {
	_, c := serveStdio(t, newFull())
	for _, ms := range []int{1, 49, 60001} {
		r := c.call("initialize", fmt.Sprintf(`{"protocols":["basalt-plugin/1"],"hooks":[],"config":{},"limits":{"timeoutMs":%d}}`, ms))
		require.NotNil(t, r.Error, ms)
		require.Equal(t, codeInvalidParams, r.Error.Code)
	}
}

func TestMalformedAndUnknownMessages(t *testing.T) {
	_, c := serveStdio(t, newFull())
	c.send(`{not json`)
	r := c.read()
	require.Equal(t, codeParse, r.Error.Code)
	require.Equal(t, "null", string(r.ID))

	r = c.call("nope", `{}`)
	require.Equal(t, codeNoMethod, r.Error.Code)

	c.send(`{"jsonrpc":"1.0","id":9,"method":"health"}`)
	r = c.read()
	require.Equal(t, codeInvalidRequest, r.Error.Code)

	c.send(`[{"jsonrpc":"2.0","id":1,"method":"health"}]`)
	r = c.read()
	require.Equal(t, codeParse, r.Error.Code, "batches are not part of the protocol")

	// A notification gets no answer; the next request is answered in order.
	c.send(`{"jsonrpc":"2.0","method":"health"}`)
	r = c.call("health", `{}`)
	require.JSONEq(t, `{"status":"READY"}`, string(r.Result))

	// A request with no params is a request with {}.
	c.send(`{"jsonrpc":"2.0","id":77,"method":"health"}`)
	r = c.read()
	require.Equal(t, "77", string(r.ID))
	require.JSONEq(t, `{"status":"READY"}`, string(r.Result))
}

func TestOversizedRequestIsRefusedBeforeDecoding(t *testing.T) {
	s, c := serveStdio(t, newFull())
	// The line is not JSON. Decoding it would give a parse error; the cap is
	// checked first, so it is refused as over the cap.
	c.send(strings.Repeat("a", MaxMessageBytes+10))
	r := c.read()
	require.NotNil(t, r.Error)
	require.Equal(t, codeInvalidRequest, r.Error.Code)
	require.Contains(t, r.Error.Message, "size cap")
	require.ErrorIs(t, s.await(t), ErrMessageTooLarge)
}

func TestMessageCapHoldsInBothDirections(t *testing.T) {
	extra := map[string]methodFunc{
		"echo": func(_ context.Context, params json.RawMessage) (any, *rpcError) { return params, nil },
		"double": func(_ context.Context, params json.RawMessage) (any, *rpcError) {
			return []json.RawMessage{params, params}, nil
		},
	}
	_, c := serveStdio(t, newFull(), withExtra(extra))

	// A request well under the cap is answered.
	big := strings.Repeat("x", 1<<20)
	r := c.call("echo", `{"s":"`+big+`"}`)
	require.Nil(t, r.Error)
	require.Len(t, r.Result, len(big)+8)

	// A request that fits is answered with an error when its answer would not
	// fit: the plugin never sends a message the engine must refuse.
	r = c.call("double", `{"s":"`+strings.Repeat("x", 3<<20)+`"}`)
	require.NotNil(t, r.Error)
	require.Contains(t, r.Error.Message, "size cap")
	// The connection is fine afterwards.
	require.Nil(t, c.call("health", `{}`).Error)
}

func TestCallsRunUnderTheEntrysDeadline(t *testing.T) {
	extra := map[string]methodFunc{"slow": func(ctx context.Context, _ json.RawMessage) (any, *rpcError) {
		<-ctx.Done()
		return nil, nil
	}}
	_, c := serveStdio(t, newFull(), withExtra(extra))
	require.Nil(t, c.call("initialize", `{"protocols":["basalt-plugin/1"],"hooks":[],"config":{},"limits":{"timeoutMs":100}}`).Error)
	start := time.Now()
	r := c.call("slow", `{}`)
	require.NotNil(t, r.Error)
	require.Equal(t, codeDeadline, r.Error.Code)
	require.Less(t, time.Since(start), 2*time.Second)
}

func TestAtMostFourCallsRunAtOnce(t *testing.T) {
	var running, peak atomic.Int32
	release := make(chan struct{})
	extra := map[string]methodFunc{"hold": func(ctx context.Context, _ json.RawMessage) (any, *rpcError) {
		n := running.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		running.Add(-1)
		return struct{}{}, nil
	}}
	_, c := serveStdio(t, newFull(), withExtra(extra))
	// Four run and the other eight wait for a slot, within the waiting cap.
	const calls = MaxInFlight + waitingCalls
	go func() {
		for i := 1; i <= calls; i++ {
			_, _ = fmt.Fprintf(c.w, `{"jsonrpc":"2.0","id":%d,"method":"hold","params":{}}`+"\n", i)
		}
	}()
	require.Eventually(t, func() bool { return running.Load() == MaxInFlight }, wait, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	require.EqualValues(t, MaxInFlight, running.Load(), "a fifth call waits for a free slot")
	close(release)
	for range calls {
		require.Nil(t, c.read().Error)
	}
	require.LessOrEqual(t, peak.Load(), int32(MaxInFlight))
}

// health and shutdown are not held behind four running hook calls: each has
// a slot of its own, as on the engine's side.
func TestLifecycleCallsAnswerWithFourCallsRunning(t *testing.T) {
	var running atomic.Int32
	release := make(chan struct{})
	extra := map[string]methodFunc{"hold": func(ctx context.Context, _ json.RawMessage) (any, *rpcError) {
		running.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return struct{}{}, nil
	}}
	_, c := serveStdio(t, newFull(), withExtra(extra))
	require.Nil(t, c.call("initialize", initParams).Error)
	for i := 1; i <= MaxInFlight; i++ {
		c.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":"h%d","method":"hold","params":{}}`, i))
	}
	require.Eventually(t, func() bool { return running.Load() == MaxInFlight }, wait, 5*time.Millisecond)
	c.send(`{"jsonrpc":"2.0","id":"health","method":"health","params":{}}`)
	r := c.read()
	require.Equal(t, `"health"`, string(r.ID), "health answered while four calls run")
	require.JSONEq(t, `{"status":"READY"}`, string(r.Result))
	c.send(`{"jsonrpc":"2.0","id":"shutdown","method":"shutdown","params":{}}`)
	r = c.read()
	require.Equal(t, `"shutdown"`, string(r.ID), "shutdown answered while four calls run")
	require.Nil(t, r.Error)
	close(release)
}

// overrunning serves "hold" with a handler that ignores its context and runs
// until the test ends, counting the handlers running.
func overrunning(t *testing.T, init string) (*stdioClient, *atomic.Int32) {
	t.Helper()
	running := new(atomic.Int32)
	release := make(chan struct{})
	extra := map[string]methodFunc{"hold": func(context.Context, json.RawMessage) (any, *rpcError) {
		running.Add(1)
		<-release
		return struct{}{}, nil
	}}
	_, c := serveStdio(t, newFull(), withExtra(extra))
	t.Cleanup(func() { close(release) }) // before Serve is stopped
	require.Nil(t, c.call("initialize", init).Error)
	for i := 1; i <= MaxInFlight; i++ {
		c.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":"h%d","method":"hold","params":{}}`, i))
	}
	require.Eventually(t, func() bool { return running.Load() == MaxInFlight }, wait, 5*time.Millisecond)
	return c, running
}

// sendAll writes lines from another goroutine: a reader that stopped reading
// must fail the test, not hang it.
func (c *stdioClient) sendAll(lines ...string) {
	go func() {
		for _, l := range lines {
			if _, err := io.WriteString(c.w, l+"\n"); err != nil {
				return
			}
		}
	}()
}

// readByID reads n replies, which may come in any order, keyed by id.
func (c *stdioClient) readByID(n int) map[string]reply {
	c.t.Helper()
	got := map[string]reply{}
	for range n {
		r := c.read()
		got[string(r.ID)] = r
	}
	return got
}

// Four hook handlers ignore their context and run past the deadline. The
// engine has timed them out, frees its slots and sends a fifth call, then
// health. The reader keeps reading: health is answered at once, and the fifth
// call is answered deadline exceeded when no slot frees in time.
func TestHealthIsAnsweredWhileHookHandlersOverrun(t *testing.T) {
	c, running := overrunning(t, shortInit)
	c.sendAll(`{"jsonrpc":"2.0","id":"h5","method":"hold","params":{}}`,
		`{"jsonrpc":"2.0","id":"health","method":"health","params":{}}`)
	got := c.readByID(2)
	require.Contains(t, got, `"health"`, "health answered behind a call waiting for a slot")
	require.Nil(t, got[`"health"`].Error)
	require.JSONEq(t, `{"status":"READY"}`, string(got[`"health"`].Result))
	require.Contains(t, got, `"h5"`, "the fifth call is answered, not left silent")
	require.NotNil(t, got[`"h5"`].Error)
	require.Equal(t, codeDeadline, got[`"h5"`].Error.Code)
	require.EqualValues(t, MaxInFlight, running.Load(), "the fifth call never ran")
}

// A peer that ignores the cap cannot make the plugin hold unbounded calls:
// past waitingCalls waiting, a call is answered busy at once, and health still
// gets through.
func TestCallsPastTheWaitingCapAreAnsweredBusy(t *testing.T) {
	const longInit = `{"protocols":["basalt-plugin/1"],"hooks":[],"config":{},"limits":{"timeoutMs":60000}}`
	c, running := overrunning(t, longInit)
	lines := make([]string, 0, waitingCalls+2)
	for i := 1; i <= waitingCalls; i++ {
		lines = append(lines, fmt.Sprintf(`{"jsonrpc":"2.0","id":"w%d","method":"hold","params":{}}`, i))
	}
	lines = append(lines, `{"jsonrpc":"2.0","id":"over","method":"hold","params":{}}`,
		`{"jsonrpc":"2.0","id":"health","method":"health","params":{}}`)
	c.sendAll(lines...)
	got := c.readByID(2)
	require.Contains(t, got, `"over"`, "the call past the cap is answered at once")
	require.NotNil(t, got[`"over"`].Error)
	require.Equal(t, codeBusy, got[`"over"`].Error.Code)
	require.Contains(t, got, `"health"`)
	require.JSONEq(t, `{"status":"READY"}`, string(got[`"health"`].Result))
	require.EqualValues(t, MaxInFlight, running.Load())
}

func TestServeRefusesAnUnnamedPlugin(t *testing.T) {
	require.ErrorIs(t, Serve(context.Background(), nil), ErrBadPlugin)
	require.ErrorIs(t, Serve(context.Background(), &fullPlugin{}), ErrBadPlugin)
	require.ErrorIs(t, Serve(context.Background(), &fullPlugin{info: Info{Name: "a"}}), ErrBadPlugin)
}

// TestConstantsMatchTheProtocol: the names and limits the SDK repeats for
// authors are the protocol's.
func TestConstantsMatchTheProtocol(t *testing.T) {
	require.Equal(t, "BASALT_PLUGIN_SOCKET", SocketEnv)
	require.Equal(t, protocol.Version1, Protocol)
	require.Equal(t, protocol.MaxMessageBytes, MaxMessageBytes)
	require.Equal(t, protocol.MaxInFlight, MaxInFlight)
}

func TestTransportChoice(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == SocketEnv {
				return v
			}
			return ""
		}
	}
	for _, tc := range []struct {
		name string
		opts []Option
		want Transport
		err  error
	}{
		{"no socket, no option: stdio", []Option{withEnv(env(""))}, JSONRPCStdio, nil},
		{"socket set: json-rpc on the socket", []Option{withEnv(env("/x/p.sock"))}, JSONRPCUnix, nil},
		{"socket set, connectrpc asked", []Option{withEnv(env("/x/p.sock")), WithTransport(ConnectRPC)}, ConnectRPC, nil},
		{"stdio asked, socket set", []Option{withEnv(env("/x/p.sock")), WithTransport(JSONRPCStdio)}, JSONRPCStdio, nil},
		{"unknown transport", []Option{WithTransport("carrier-pigeon")}, "", ErrTransport},
		{"token on a json-rpc transport", []Option{WithBearerToken("t")}, "", ErrTransport},
	} {
		got, err := newConfig(tc.opts).resolve()
		require.ErrorIs(t, err, tc.err, tc.name)
		require.Equal(t, tc.want, got, tc.name)
	}
}

func TestSocketTransportsNeedAnAbsoluteSocketPath(t *testing.T) {
	for _, tr := range []Transport{JSONRPCUnix, ConnectRPC} {
		for _, v := range []string{"", "rel/p.sock"} {
			err := Serve(context.Background(), newFull(), WithTransport(tr), withEnv(func(string) string { return v }))
			require.ErrorIs(t, err, ErrNoSocket, "%s %q", tr, v)
		}
	}
}

func TestOnlyTheSocketVariableIsRead(t *testing.T) {
	var asked []string
	get := func(k string) string {
		asked = append(asked, k)
		return ""
	}
	_, c := serveStdio(t, newFull(), withEnv(get))
	c.call("health", `{}`)
	require.Subset(t, []string{SocketEnv}, asked)
}

// A socket left at the path by a crash (a service unit's directory stays
// between starts) is replaced; anything else there is refused.
func TestListenReplacesAStaleSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "sk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := dir + "/p.sock"
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()
	cfg := newConfig([]Option{withEnv(func(string) string { return path })})
	l, err := listenUnix(cfg)
	if err != nil {
		t.Fatalf("a stale socket is replaced: %v", err)
	}
	_ = l.Close()

	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnix(cfg); err == nil {
		t.Fatal("a file that is not a socket is left and refused")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "x" {
		t.Fatalf("the file was changed: %q %v", b, err)
	}
}
