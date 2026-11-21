package plugintool

import (
	"context"
	"fmt"
	"net"
	"net/http"

	connectrpc "connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/basaltdbhq/basalt-plugin-go/proto/basalt/plugin/v1"
	"github.com/basaltdbhq/basalt-plugin-go/proto/basalt/plugin/v1/pluginv1connect"
	"github.com/basaltdbhq/basalt-plugin-go/protocol"
)

// connectClient speaks gRPC over h2c on a Unix socket, as the engine does for
// a local connectrpc plugin.
type connectClient struct {
	c      pluginv1connect.PluginServiceClient
	closer func()
}

func newConnectClient(socket string, closer func()) *connectClient {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	tr.Protocols = &protocols
	hc := &http.Client{Transport: tr}
	return &connectClient{
		c: pluginv1connect.NewPluginServiceClient(hc, "http://plugin", connectrpc.WithGRPC(),
			connectrpc.WithSendMaxBytes(protocol.MaxMessageBytes), connectrpc.WithReadMaxBytes(protocol.MaxMessageBytes)),
		closer: func() { tr.CloseIdleConnections(); closer() },
	}
}

func (c *connectClient) close() { c.closer() }

func (c *connectClient) initialize(ctx context.Context, req protocol.InitializeRequest) (protocol.InitializeResponse, error) {
	cfg, err := structpb.NewStruct(map[string]any{})
	if err != nil {
		return protocol.InitializeResponse{}, err
	}
	res, err := c.c.Initialize(ctx, connectrpc.NewRequest(&pluginv1.InitializeRequest{
		Protocols: req.Protocols, Hooks: req.Hooks, Config: cfg,
		Limits: &pluginv1.Limits{
			MaxMessageBytes: int32(req.Limits.MaxMessageBytes), MaxInFlight: int32(req.Limits.MaxInFlight), TimeoutMs: int32(req.Limits.TimeoutMs),
		},
	}))
	if err != nil {
		return protocol.InitializeResponse{}, connectFailure("initialize", err)
	}
	m := res.Msg
	out := protocol.InitializeResponse{Protocol: m.GetProtocol(), Name: m.GetName(), Version: m.GetVersion(), Hooks: map[string]protocol.HookDecl{}}
	for name, d := range m.GetHooks() {
		out.Hooks[name] = protocol.HookDecl{Contract: d.GetContract(), Roles: d.GetRoles(), EntityTypes: d.GetEntityTypes()}
	}
	return out, nil
}

func (c *connectClient) health(ctx context.Context) (protocol.HealthResponse, error) {
	res, err := c.c.Health(ctx, connectrpc.NewRequest(&pluginv1.HealthRequest{}))
	if err != nil {
		return protocol.HealthResponse{}, connectFailure("health", err)
	}
	return protocol.HealthResponse{Status: res.Msg.GetStatus()}, nil
}

func (c *connectClient) shutdown(ctx context.Context) error {
	_, err := c.c.Shutdown(ctx, connectrpc.NewRequest(&pluginv1.ShutdownRequest{}))
	return connectFailure("shutdown", err)
}

func (c *connectClient) feedObserve(ctx context.Context, req protocol.FeedObserveRequest) (int64, error) {
	res, err := c.c.FeedObserve(ctx, connectrpc.NewRequest(&pluginv1.FeedObserveRequest{Agent: req.Agent, Events: req.Events}))
	if err != nil {
		return 0, connectFailure(protocol.MethodFeedObserve, err)
	}
	return res.Msg.GetLastSeq(), nil
}

func (c *connectClient) approvalDeliver(ctx context.Context, req protocol.ApprovalDeliverRequest) error {
	_, err := c.c.ApprovalDeliver(ctx, connectrpc.NewRequest(&pluginv1.ApprovalDeliverRequest{Agent: req.Agent, Event: req.Event}))
	return connectFailure(protocol.MethodApprovalDeliver, err)
}

// connectFailure is err as a plain error naming the call; nil stays nil.
func connectFailure(call string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: the plugin answered an error: %s", call, connectrpc.CodeOf(err))
}
