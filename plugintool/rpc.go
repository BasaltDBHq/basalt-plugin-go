package plugintool

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/basaltdbhq/basalt-plugin-go/protocol"
)

// caller is one connection to a plugin, whatever its transport.
type caller interface {
	initialize(ctx context.Context, req protocol.InitializeRequest) (protocol.InitializeResponse, error)
	health(ctx context.Context) (protocol.HealthResponse, error)
	shutdown(ctx context.Context) error
	feedObserve(ctx context.Context, req protocol.FeedObserveRequest) (int64, error)
	approvalDeliver(ctx context.Context, req protocol.ApprovalDeliverRequest) error
	close()
}

// rpcClient speaks newline-delimited JSON-RPC 2.0 on a stream, one call at a
// time. A call that ends with its context leaves the connection unusable; the
// caller then stops the plugin.
type rpcClient struct {
	w      io.Writer
	r      *bufio.Reader
	closer func()
	next   int
}

type rpcReply struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func newRPCClient(r io.Reader, w io.Writer, closer func()) *rpcClient {
	return &rpcClient{w: w, r: bufio.NewReaderSize(r, readBuffer), closer: closer}
}

const readBuffer = 64 << 10

func (c *rpcClient) close() { c.closer() }

func (c *rpcClient) call(ctx context.Context, method string, params, result any) error {
	c.next++
	id := c.next
	line, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", id, method, params})
	if err != nil {
		return err
	}
	type outcome struct {
		res json.RawMessage
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		if _, err := c.w.Write(append(line, '\n')); err != nil {
			done <- outcome{err: errors.New("the plugin's input is closed")}
			return
		}
		res, err := c.readReply(id)
		done <- outcome{res, err}
	}()
	select {
	case <-ctx.Done():
		return fmt.Errorf("%s: no answer in time", method)
	case o := <-done:
		if o.err != nil {
			return fmt.Errorf("%s: %w", method, o.err)
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(o.res, result); err != nil {
			return fmt.Errorf("%s: the answer is not the expected shape", method)
		}
		return nil
	}
}

func (c *rpcClient) readReply(id int) (json.RawMessage, error) {
	want := fmt.Sprint(id)
	for {
		b, err := c.r.ReadBytes('\n')
		if err != nil {
			return nil, errors.New("the plugin closed its output before it answered")
		}
		var rep rpcReply
		if json.Unmarshal(b, &rep) != nil || string(rep.ID) != want {
			continue
		}
		if rep.Error != nil {
			return nil, fmt.Errorf("the plugin answered an error (code %d): %s", rep.Error.Code, rep.Error.Message)
		}
		return rep.Result, nil
	}
}

func (c *rpcClient) initialize(ctx context.Context, req protocol.InitializeRequest) (protocol.InitializeResponse, error) {
	var out protocol.InitializeResponse
	err := c.call(ctx, protocol.MethodInitialize, req, &out)
	return out, err
}

func (c *rpcClient) health(ctx context.Context) (protocol.HealthResponse, error) {
	var out protocol.HealthResponse
	err := c.call(ctx, protocol.MethodHealth, protocol.HealthRequest{}, &out)
	return out, err
}

func (c *rpcClient) shutdown(ctx context.Context) error {
	return c.call(ctx, protocol.MethodShutdown, protocol.ShutdownRequest{}, nil)
}

func (c *rpcClient) feedObserve(ctx context.Context, req protocol.FeedObserveRequest) (int64, error) {
	var out protocol.FeedObserveResponse
	err := c.call(ctx, protocol.MethodFeedObserve, req, &out)
	return out.LastSeq, err
}

func (c *rpcClient) approvalDeliver(ctx context.Context, req protocol.ApprovalDeliverRequest) error {
	return c.call(ctx, protocol.MethodApprovalDeliver, req, nil)
}
