package basaltplugin

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/BasaltDBHq/basalt-plugin-go/proto/basalt/plugin/v1"
	"github.com/BasaltDBHq/basalt-plugin-go/proto/basalt/plugin/v1/pluginv1connect"
	"github.com/BasaltDBHq/basalt-plugin-go/protocol"
)

// detectHook is a declaration with every field set, so a field the SDK drops
// on one transport shows up as a difference.
var detectHook = map[string]HookDecl{"wire.detect": {
	Contract: "detector/1", Roles: []string{"VALUE", "NAME_CUE"}, EntityTypes: []string{"PERSON_NAME", "acme-feed.ID"},
}}

func fullRequest() protocol.InitializeRequest {
	return protocol.InitializeRequest{
		Protocols: []string{"basalt-plugin/2", "basalt-plugin/1"},
		Hooks:     []string{"wire.detect"},
		Config:    protocol.RawObject(`{"threshold":0.5,"names":["a","b"],"nested":{"on":true}}`),
		Limits:    protocol.Limits{MaxMessageBytes: protocol.MaxMessageBytes, MaxInFlight: protocol.MaxInFlight, TimeoutMs: 1500},
	}
}

func wantResponse() protocol.InitializeResponse {
	return protocol.InitializeResponse{
		Protocol: protocol.Version1, Name: "acme-feed", Version: "2.1.0", Hooks: detectHook,
		Model: &protocol.Model{Name: "tiny", Version: "3"},
	}
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	t.Cleanup(cancel)
	return ctx
}

// engineSide dials the SDK plugin as the engine does on one socket transport.
type engineSide struct {
	name string
	opts []Option
	dial func(ctx context.Context, path string) (wire, error)
	// refused reports whether err is the plugin's refusal of a call, in the
	// transport's own code.
	refused func(err error) bool
}

var engineSides = []engineSide{
	{"jsonrpc-unix", nil, dialJSONRPC, func(err error) bool {
		var r *refusal
		return errors.As(err, &r) && r.code == codeRefused
	}},
	{"connectrpc", []Option{WithTransport(ConnectRPC)}, dialConnect, func(err error) bool {
		return connectrpc.CodeOf(err) == connectrpc.CodeFailedPrecondition
	}},
}

// TestLifecycleOnEachSocketTransport: the whole lifecycle runs against an SDK
// plugin on each socket transport, and every field of initialize survives the
// trip both ways.
func TestLifecycleOnEachSocketTransport(t *testing.T) {
	for _, side := range engineSides {
		t.Run(side.name, func(t *testing.T) {
			p := newFull()
			s, path := serveSocket(t, p, append([]Option{WithHooks(detectHook)}, side.opts...)...)
			tr := dialT(t, side.dial, path)

			var res protocol.InitializeResponse
			require.NoError(t, tr.Call(ctxT(t), protocol.MethodInitialize, fullRequest(), &res))
			require.Equal(t, wantResponse(), res)
			in := p.gotInit.Load()
			require.NotNil(t, in)
			require.JSONEq(t, `{"threshold":0.5,"names":["a","b"],"nested":{"on":true}}`, string(in.Config))
			require.Equal(t, 1500, in.TimeoutMs)
			require.Equal(t, protocol.Version1, in.Protocol)

			var h protocol.HealthResponse
			require.NoError(t, tr.Call(ctxT(t), protocol.MethodHealth, protocol.HealthRequest{}, &h))
			require.Equal(t, protocol.HealthReady, h.Status)
			p.unhealthy.Store(true)
			require.NoError(t, tr.Call(ctxT(t), protocol.MethodHealth, protocol.HealthRequest{}, &h))
			require.Equal(t, protocol.HealthNotReady, h.Status)

			start := time.Now()
			require.NoError(t, tr.Call(ctxT(t), protocol.MethodShutdown, protocol.ShutdownRequest{}, nil))
			require.NoError(t, s.await(t), "shutdown ends Serve cleanly")
			require.Less(t, time.Since(start), 4*time.Second, "Serve does not wait out its grace period and then some")
			require.EqualValues(t, 1, p.shutdownIn.Load())
		})
	}
}

// TestUnknownProtocolIsRefusedOnEveryTransport: the engine sees a failed call,
// never a plugin that answers with a protocol it was not offered.
func TestUnknownProtocolIsRefusedOnEveryTransport(t *testing.T) {
	for _, side := range engineSides {
		t.Run(side.name, func(t *testing.T) {
			_, path := serveSocket(t, newFull(), append([]Option{WithHooks(detectHook)}, side.opts...)...)
			tr := dialT(t, side.dial, path)
			req := fullRequest()
			req.Protocols = []string{"basalt-plugin/2"}
			var res protocol.InitializeResponse
			err := tr.Call(ctxT(t), protocol.MethodInitialize, req, &res)
			require.True(t, side.refused(err), "%v", err)
			require.Empty(t, res.Name)
			// A later good offer still works on the same connection.
			require.NoError(t, tr.Call(ctxT(t), protocol.MethodInitialize, fullRequest(), &res))
		})
	}
}

func TestJSONRPCSocketServesSeveralConnections(t *testing.T) {
	p := newFull()
	s, path := serveSocket(t, p, WithHooks(detectHook))
	trs := make([]wire, 0, 3)
	for range 3 {
		trs = append(trs, dialT(t, dialJSONRPC, path))
	}
	for _, tr := range trs {
		var res protocol.InitializeResponse
		require.NoError(t, tr.Call(ctxT(t), protocol.MethodInitialize, fullRequest(), &res))
		require.Equal(t, wantResponse(), res)
	}
	// Shutdown on any connection ends the plugin.
	require.NoError(t, trs[1].Call(ctxT(t), protocol.MethodShutdown, protocol.ShutdownRequest{}, nil))
	require.NoError(t, s.await(t))
}

// TestPlainGRPCAndConnectClientsWork: the local ConnectRPC server is h2c, so a
// stock gRPC client (or a Connect one) needs nothing of the engine.
func TestPlainGRPCAndConnectClientsWork(t *testing.T) {
	_, path := serveSocket(t, newFull(), WithTransport(ConnectRPC), WithHooks(detectHook))
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	hc := &http.Client{Transport: &http.Transport{
		Protocols: &protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
	t.Cleanup(hc.CloseIdleConnections)

	for name, opt := range map[string]connectrpc.ClientOption{
		"grpc": connectrpc.WithGRPC(), "grpc-web": connectrpc.WithGRPCWeb(), "connect": connectrpc.WithProtoJSON(),
	} {
		t.Run(name, func(t *testing.T) {
			cl := pluginv1connect.NewPluginServiceClient(hc, "http://plugin", opt)
			cfg, err := structpb.NewStruct(map[string]any{"k": "v"})
			require.NoError(t, err)
			res, err := cl.Initialize(ctxT(t), connectrpc.NewRequest(&pluginv1.InitializeRequest{
				Protocols: []string{"basalt-plugin/1"}, Hooks: []string{"wire.detect"}, Config: cfg,
				Limits: &pluginv1.Limits{MaxMessageBytes: protocol.MaxMessageBytes, MaxInFlight: 4, TimeoutMs: 1000},
			}))
			require.NoError(t, err)
			require.Equal(t, "acme-feed", res.Msg.GetName())
			require.Equal(t, []string{"VALUE", "NAME_CUE"}, res.Msg.GetHooks()["wire.detect"].GetRoles())
			require.Equal(t, "tiny", res.Msg.GetModel().GetName())

			h, err := cl.Health(ctxT(t), connectrpc.NewRequest(&pluginv1.HealthRequest{}))
			require.NoError(t, err)
			require.Equal(t, "READY", h.Msg.GetStatus())

			_, err = cl.Initialize(ctxT(t), connectrpc.NewRequest(&pluginv1.InitializeRequest{Protocols: []string{"basalt-plugin/9"}}))
			require.Equal(t, connectrpc.CodeFailedPrecondition, connectrpc.CodeOf(err))
		})
	}
}

func TestConnectRefusesAnOversizedRequestUnread(t *testing.T) {
	p := newFull()
	_, path := serveSocket(t, p, WithTransport(ConnectRPC))
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
	cfg, err := structpb.NewStruct(map[string]any{"s": strings.Repeat("x", MaxMessageBytes+1024)})
	require.NoError(t, err)
	_, err = cl.Initialize(ctxT(t), connectrpc.NewRequest(&pluginv1.InitializeRequest{Protocols: []string{"basalt-plugin/1"}, Config: cfg}))
	require.Equal(t, connectrpc.CodeResourceExhausted, connectrpc.CodeOf(err))
	require.Nil(t, p.gotInit.Load(), "the plugin never saw the message")
}

// selfSigned returns a certificate for 127.0.0.1 and a pool that trusts it.
func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "plugin"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func TestRemoteConnectOverTLSWithABearerToken(t *testing.T) {
	cert, pool := selfSigned(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := serveBackground(newFull(), WithListener(l, &tls.Config{Certificates: []tls.Certificate{cert}}),
		WithBearerToken("s3cret"), WithHooks(detectHook))
	t.Cleanup(s.cancel)

	hc := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true,
	}}
	t.Cleanup(hc.CloseIdleConnections)
	cl := pluginv1connect.NewPluginServiceClient(hc, "https://"+l.Addr().String(), connectrpc.WithGRPC())

	// No token, wrong token: refused, nothing answered.
	_, err = cl.Health(ctxT(t), connectrpc.NewRequest(&pluginv1.HealthRequest{}))
	require.Error(t, err)
	req := connectrpc.NewRequest(&pluginv1.HealthRequest{})
	req.Header().Set("Authorization", "Bearer wrong")
	_, err = cl.Health(ctxT(t), req)
	require.Error(t, err)

	req = connectrpc.NewRequest(&pluginv1.HealthRequest{})
	req.Header().Set("Authorization", "Bearer s3cret")
	res, err := cl.Health(ctxT(t), req)
	require.NoError(t, err)
	require.Equal(t, "READY", res.Msg.GetStatus())

	// A client that does not trust the certificate cannot talk to it.
	bad := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true}}
	t.Cleanup(bad.CloseIdleConnections)
	_, err = pluginv1connect.NewPluginServiceClient(bad, "https://"+l.Addr().String(), connectrpc.WithGRPC()).
		Health(ctxT(t), connectrpc.NewRequest(&pluginv1.HealthRequest{}))
	require.Error(t, err)

	down := connectrpc.NewRequest(&pluginv1.ShutdownRequest{})
	down.Header().Set("Authorization", "Bearer s3cret")
	_, err = cl.Shutdown(ctxT(t), down)
	require.NoError(t, err)
	require.NoError(t, s.await(t))
}

func TestListenerNeedsTLSUnlessItIsASocket(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	require.ErrorIs(t, Serve(context.Background(), newFull(), WithListener(l, nil)), ErrTLSRequired)
	require.ErrorIs(t, Serve(context.Background(), newFull(), WithListener(l, &tls.Config{}), WithTransport(JSONRPCUnix)), ErrTransport)
}

func TestUnixListenerSuppliedByThePlugin(t *testing.T) {
	path := socketPath(t)
	l, err := net.Listen("unix", path)
	require.NoError(t, err)
	s := serveBackground(newFull(), WithListener(l, nil), WithHooks(detectHook))
	t.Cleanup(s.cancel)
	tr := dialT(t, dialConnect, path)
	var res protocol.InitializeResponse
	require.NoError(t, tr.Call(ctxT(t), protocol.MethodInitialize, fullRequest(), &res))
	require.Equal(t, wantResponse(), res)
	require.NoError(t, s.stop(t))
}

// TestPeerExitsZeroOnShutdown: the process built from the SDK exits 0 when the
// engine sends shutdown, on stdio and on a socket.
func TestPeerExitsZeroOnShutdown(t *testing.T) {
	t.Run("stdio", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], peerFlag, "echo")
		cmd.Env = []string{}
		in, err := cmd.StdinPipe()
		require.NoError(t, err)
		out, err := cmd.StdoutPipe()
		require.NoError(t, err)
		require.NoError(t, cmd.Start())
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		_, err = io.WriteString(in, `{"jsonrpc":"2.0","id":1,"method":"shutdown"}`+"\n")
		require.NoError(t, err)
		line, err := bufio.NewReader(out).ReadString('\n')
		require.NoError(t, err)
		require.JSONEq(t, `{"jsonrpc":"2.0","id":1,"result":{}}`, line)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			require.NoError(t, err, "exit status 0")
		case <-time.After(wait):
			t.Fatal("the plugin did not exit")
		}
	})
	t.Run("socket", func(t *testing.T) {
		path := socketPath(t)
		cmd := peerProc(t, "echo", SocketEnv+"="+path)
		waitSocket(t, path)
		tr := dialT(t, dialJSONRPC, path)
		require.NoError(t, tr.Call(ctxT(t), protocol.MethodShutdown, protocol.ShutdownRequest{}, nil))
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			require.NoError(t, err, "exit status 0")
		case <-time.After(wait):
			t.Fatal("the plugin did not exit")
		}
	})
}

// failingListener hands out one connection, then fails the next Accept with
// a reason that is not the context ending. Close is recorded.
type failingListener struct {
	conns  chan net.Conn
	closed chan struct{}
}

func (l *failingListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	default:
		return nil, io.ErrUnexpectedEOF
	}
}

func (l *failingListener) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}
func (l *failingListener) Addr() net.Addr { return &net.UnixAddr{Name: "x", Net: "unix"} }

// A failed Accept cancels the connections still open before it waits for
// them, so Serve returns the accept error instead of hanging on an idle peer.
func TestListenerAcceptFailureEndsOpenConnections(t *testing.T) {
	s, err := newServer(newFull(), &config{})
	require.NoError(t, err)
	server, peer := net.Pipe()
	defer peer.Close()
	l := &failingListener{conns: make(chan net.Conn, 1), closed: make(chan struct{})}
	l.conns <- server

	done := make(chan error, 1)
	go func() { done <- s.serveJSONRPCListener(context.Background(), l) }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	case <-time.After(5 * time.Second):
		t.Fatal("serveJSONRPCListener hung on an open connection after Accept failed")
	}
}
