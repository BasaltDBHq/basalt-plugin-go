package basaltplugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	connectrpc "connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/BasaltDBHq/basalt-plugin-go/proto/basalt/plugin/v1"
	"github.com/BasaltDBHq/basalt-plugin-go/proto/basalt/plugin/v1/pluginv1connect"
	"github.com/BasaltDBHq/basalt-plugin-go/protocol"
)

// wire is the engine's side of one socket connection to a plugin, as far as
// these tests need it: it sends a method's params (a protocol type) and fills
// result from the reply. The engine has its own adapters; these stand in for
// them, so the tests need nothing outside this module.
type wire interface {
	Call(ctx context.Context, method string, params, result any) error
	Close() error
}

// refusal is a JSON-RPC error reply.
type refusal struct{ code int }

func (r *refusal) Error() string { return fmt.Sprintf("the plugin answered error %d", r.code) }

// jsonrpcWire speaks newline-delimited JSON-RPC on a Unix socket, one call
// at a time.
type jsonrpcWire struct {
	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
	next int
}

func dialJSONRPC(ctx context.Context, path string) (wire, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	return &jsonrpcWire{conn: conn, r: bufio.NewReaderSize(conn, MaxMessageBytes+4096)}, nil
}

func (w *jsonrpcWire) Call(ctx context.Context, method string, params, result any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	body, err := paramsJSON(params)
	if err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := w.conn.SetDeadline(deadline); err != nil {
			return err
		}
	}
	w.next++
	if _, err := fmt.Fprintf(w.conn, `{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`+"\n", w.next, method, body); err != nil {
		return err
	}
	line, err := w.r.ReadBytes('\n')
	if err != nil {
		return err
	}
	var r reply
	if err := json.Unmarshal(line, &r); err != nil {
		return err
	}
	if string(r.ID) != fmt.Sprint(w.next) {
		return errors.New("the reply carries another call's id")
	}
	if r.Error != nil {
		return &refusal{code: r.Error.Code}
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(r.Result, result)
}

func (w *jsonrpcWire) Close() error { return w.conn.Close() }

// connectWire calls basalt.plugin.v1.PluginService with the generated client,
// over h2c on a Unix socket. Each one has a connection of its own.
type connectWire struct {
	hc *http.Client
	cl pluginv1connect.PluginServiceClient
}

func dialConnect(_ context.Context, path string) (wire, error) {
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	hc := &http.Client{Transport: &http.Transport{
		Protocols: &protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
	return &connectWire{hc: hc, cl: pluginv1connect.NewPluginServiceClient(hc, "http://plugin", connectrpc.WithGRPC())}, nil
}

func (w *connectWire) Close() error {
	w.hc.CloseIdleConnections()
	return nil
}

// Call maps the params' JSON form to the method's message, as the contract
// says: the JSON names are the proto3 JSON names, and a notifier hook's
// events are bytes that hold the events' own JSON.
func (w *connectWire) Call(ctx context.Context, method string, params, result any) error {
	js, err := paramsJSON(params)
	if err != nil {
		return err
	}
	var out []byte
	switch method {
	case protocol.MethodInitialize:
		req := new(pluginv1.InitializeRequest)
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(js, req); err != nil {
			return err
		}
		out, err = unary(ctx, w.cl.Initialize, req, protoJSON[*pluginv1.InitializeResponse])
	case protocol.MethodHealth:
		out, err = unary(ctx, w.cl.Health, new(pluginv1.HealthRequest), protoJSON[*pluginv1.HealthResponse])
	case protocol.MethodShutdown:
		out, err = unary(ctx, w.cl.Shutdown, new(pluginv1.ShutdownRequest), protoJSON[*pluginv1.ShutdownResponse])
	case protocol.MethodFeedObserve:
		var in protocol.FeedObserveRequest
		if err := json.Unmarshal(js, &in); err != nil {
			return err
		}
		// proto3 JSON would write the 64-bit seq as a string.
		out, err = unary(ctx, w.cl.FeedObserve, &pluginv1.FeedObserveRequest{Agent: in.Agent, Events: in.Events},
			func(m *pluginv1.FeedObserveResponse) ([]byte, error) {
				return json.Marshal(protocol.FeedObserveResponse{LastSeq: m.GetLastSeq()})
			})
	case protocol.MethodApprovalDeliver:
		var in protocol.ApprovalDeliverRequest
		if err := json.Unmarshal(js, &in); err != nil {
			return err
		}
		out, err = unary(ctx, w.cl.ApprovalDeliver, &pluginv1.ApprovalDeliverRequest{Agent: in.Agent, Event: in.Event},
			protoJSON[*pluginv1.ApprovalDeliverResponse])
	default:
		return errors.New("no such method on the service: " + method)
	}
	if err != nil || result == nil {
		return err
	}
	return json.Unmarshal(out, result)
}

func protoJSON[M proto.Message](m M) ([]byte, error) { return protojson.Marshal(m) }

// unary makes one call and renders the reply as JSON.
func unary[Req, Res any](
	ctx context.Context,
	call func(context.Context, *connectrpc.Request[Req]) (*connectrpc.Response[Res], error),
	req *Req, render func(*Res) ([]byte, error),
) ([]byte, error) {
	res, err := call(ctx, connectrpc.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return render(res.Msg)
}

func paramsJSON(params any) ([]byte, error) {
	if params == nil {
		return []byte("{}"), nil
	}
	return protocol.Marshal(params)
}

// Fixtures for the notifier hooks.

// feedEvent is one feed event of type write.applied with the given seq, in
// its JSON form, with a table name that holds characters JSON may escape.
func feedEvent(seq int64) string {
	return fmt.Sprintf(`{"specversion":"1.0","id":"0193a0f2-5b7e-7c41-9d2a-6e1f00c0ffee:%d",`+
		`"source":"urn:uuid:0193a0f2-5b7e-7c41-9d2a-6e1f00c0ffee","type":"dev.basaltdb.write.applied",`+
		`"time":"2026-11-19T06:02:11Z","subject":"product_a","datacontenttype":"application/json",`+
		`"data":{"installId":"0193a0f2-5b7e-7c41-9d2a-6e1f00c0ffee","seq":%d,"agent":"product_a",`+
		`"changeId":"0193a1b3-0000-7000-8000-000000000001","op":"UPDATE","table":"public.a<b&c>","rows":1,"cascadedRows":0}}`, seq, seq)
}

// feedBatch is a feed batch of the given events' JSON forms.
func feedBatch(events ...string) json.RawMessage {
	return json.RawMessage("[" + strings.Join(events, ",") + "]")
}

// noticeEvent is an approval.deliver notice with no link.
const noticeEvent = `{"specversion":"1.0","id":"0193a1b4-0000-7000-8000-000000000001",` +
	`"source":"urn:uuid:0193a0f2-5b7e-7c41-9d2a-6e1f00c0ffee","type":"dev.basaltdb.approval.requested",` +
	`"time":"2026-11-19T06:03:02Z","subject":"product_a","datacontenttype":"application/json",` +
	`"data":{"kind":"APPROVAL_REQUESTED","agent":"product_a","approvalId":"0193a1b3-1111-7000-8000-000000000001",` +
	`"headline":"Update 27 rows in public·leads","expiresAt":"2099-01-01T00:00:00Z",` +
	`"instruction":"Ask the agent for the link to review the held write and approve it with your passkey."}}`

// dialT dials and closes the connection when the test ends.
func dialT(t *testing.T, dial func(context.Context, string) (wire, error), path string) wire {
	t.Helper()
	w, err := dial(ctxT(t), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}
