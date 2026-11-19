package basaltplugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/BasaltDBHq/basalt-plugin-go/protocol"
)

const (
	jsonrpcVersion = "2.0"
	// readBufferBytes is the size of the buffered reader; a longer line is
	// assembled from several reads, never beyond the message cap.
	readBufferBytes = 64 << 10
	// closeWait bounds the wait for a reader that closing its stream did not
	// unblock (a terminal, for instance).
	closeWait = 500 * time.Millisecond
)

var errTooLong = errors.New("sdk: line over the size cap")

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcBody        `json:"error,omitempty"`
}

// serveConn serves one JSON-RPC connection until the reader ends, the stream
// breaks, or ctx ends. It closes rc. It returns ErrMessageTooLarge when the
// peer sent a line over the cap, nil in every other case.
//
// Calls are served concurrently, at most MaxInFlight at a time. The reader
// never waits for a hook call's slot: each call waits for one in its own
// goroutine, under its own deadline, so a health line behind it is still read
// while hook handlers run past their deadline. At most waitingCalls more wait;
// past that a call is answered errBusy at once. Replies may arrive out of
// order; each carries its request's id.
func (s *server) serveConn(ctx context.Context, rc io.ReadCloser, w io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	c := &jsonrpcConn{
		s: s, w: w,
		slots:    make(chan struct{}, protocol.MaxInFlight),
		admitted: make(chan struct{}, protocol.MaxInFlight+waitingCalls),
		health:   make(chan struct{}, 1), shutdown: make(chan struct{}, 1),
	}
	readDone := make(chan error, 1)
	go func() { readDone <- c.readLoop(ctx, rc) }()

	var err error
	select {
	case err = <-readDone:
		// The peer closed its side. Calls already running finish and are
		// answered, bounded by the call timeout; a cancelled parent ctx or a
		// stuck handler ends the wait.
		drain := time.NewTimer(time.Duration(s.timeoutNs.Load()) + closeWait)
		handlers := make(chan struct{})
		go func() { c.wg.Wait(); close(handlers) }()
		select {
		case <-handlers:
		case <-ctx.Done():
		case <-drain.C:
		}
		drain.Stop()
	case <-ctx.Done():
		_ = rc.Close()
		select {
		case err = <-readDone:
		case <-time.After(closeWait):
		}
	}
	if !errors.Is(err, ErrMessageTooLarge) {
		err = nil // an ended stream is a clean end
	}
	cancel()
	_ = rc.Close()
	// Handlers see the cancelled ctx. One stuck writing to a peer that no
	// longer reads is abandoned after a moment: the process is ending.
	handlers := make(chan struct{})
	go func() { c.wg.Wait(); close(handlers) }()
	select {
	case <-handlers:
	case <-time.After(closeWait):
	}
	return err
}

// waitingCalls is how many calls may wait for a slot beyond those running.
// The engine keeps at most MaxInFlight hook calls open, but a call it gave up
// on still waits here until the SDK's own deadline for it, so more than
// MaxInFlight may wait. Each one ends at its deadline; the cap only bounds
// what a burst can hold.
const waitingCalls = 2 * protocol.MaxInFlight

type jsonrpcConn struct {
	s   *server
	w   io.Writer
	wmu sync.Mutex
	// slots caps the calls running at once; health and shutdown each have
	// one slot of their own outside it, as on the engine's side, so they are
	// answered while four hook calls run. A handler that ignores its context
	// keeps its slot until it returns.
	slots chan struct{}
	// admitted caps the calls holding or waiting for one of slots.
	admitted chan struct{}
	health   chan struct{}
	shutdown chan struct{}
	wg       sync.WaitGroup
}

// slotsFor is the slots a request line runs under, by its method. A line
// that does not decode runs under the common slots, where handle answers it.
func (c *jsonrpcConn) slotsFor(line []byte) chan struct{} {
	var req struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(line, &req) != nil {
		return c.slots
	}
	switch req.Method {
	case protocol.MethodHealth:
		return c.health
	case protocol.MethodShutdown:
		return c.shutdown
	}
	return c.slots
}

func (c *jsonrpcConn) readLoop(ctx context.Context, r io.Reader) error {
	br := bufio.NewReaderSize(r, readBufferBytes)
	for {
		line, err := readLine(br, protocol.MaxMessageBytes)
		if errors.Is(err, errTooLong) {
			// Refused before any of it is decoded. The rest of the line is
			// unread, so the stream cannot be trusted again.
			c.write(rpcResponse{JSONRPC: jsonrpcVersion, Error: &rpcBody{Code: codeInvalidRequest, Message: "message over the size cap"}})
			return ErrMessageTooLarge
		}
		if err != nil {
			return err // the peer closed the stream, or ctx closed it
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 {
			continue
		}
		slots := c.slotsFor(line)
		if slots != c.slots {
			// health or shutdown: its own slot, held at most until the
			// deadline of the one call running, since both are bounded.
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return nil
			}
			c.wg.Add(1)
			go func() {
				defer c.wg.Done()
				defer func() { <-slots }()
				c.handle(ctx, line, nil)
			}()
			continue
		}
		select {
		case c.admitted <- struct{}{}:
		default:
			// Too many calls wait already: answer at once and read on.
			if req, notification, ok := c.request(line); ok {
				c.reply(req.ID, notification, nil, errBusy)
			}
			continue
		}
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			defer func() { <-c.admitted }()
			c.handle(ctx, line, c.slots)
		}()
	}
}

// request decodes one line. A line that is not a valid request is answered
// here, and ok is false.
func (c *jsonrpcConn) request(line []byte) (req rpcRequest, notification, ok bool) {
	if err := json.Unmarshal(line, &req); err != nil {
		c.write(rpcResponse{JSONRPC: jsonrpcVersion, Error: &rpcBody{Code: codeParse, Message: "parse error"}})
		return req, false, false
	}
	notification = len(req.ID) == 0
	if !notification && !validID(req.ID) {
		c.write(rpcResponse{JSONRPC: jsonrpcVersion, Error: &rpcBody{Code: codeInvalidRequest, Message: "invalid request"}})
		return req, false, false
	}
	if req.JSONRPC != jsonrpcVersion || req.Method == "" {
		c.reply(req.ID, notification, nil, &rpcError{codeInvalidRequest, "invalid request"})
		return req, false, false
	}
	return req, notification, true
}

// handle answers one line. With slots, the call first waits for one of them.
// The wait counts against its deadline, which starts when the line is read,
// so a call that gets no slot in time is answered deadline exceeded.
func (c *jsonrpcConn) handle(ctx context.Context, line []byte, slots chan struct{}) {
	callCtx, cancel := c.s.callContext(ctx)
	defer cancel()
	req, notification, ok := c.request(line)
	if !ok {
		return
	}
	if slots != nil {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-callCtx.Done():
			c.reply(req.ID, notification, nil, errDeadline)
			return
		}
	}
	params := req.Params
	if len(params) == 0 || bytes.Equal(params, []byte("null")) {
		params = json.RawMessage("{}")
	}
	res, e := c.s.dispatch(callCtx, req.Method, params)
	cancel()
	c.reply(req.ID, notification, res, e)
	if req.Method == protocol.MethodShutdown && e == nil {
		c.s.requestStop()
	}
}

func validID(id json.RawMessage) bool {
	return len(id) > 0 && (id[0] == '"' || id[0] == '-' || (id[0] >= '0' && id[0] <= '9'))
}

func (c *jsonrpcConn) reply(id json.RawMessage, notification bool, res any, e *rpcError) {
	if notification {
		return
	}
	out := rpcResponse{JSONRPC: jsonrpcVersion, ID: id}
	if e != nil {
		out.Error = &rpcBody{Code: e.code, Message: e.msg}
	} else {
		out.Result = res
	}
	c.write(out)
}

// write sends one response line. A result over the size cap is replaced by an
// error: the SDK never sends the engine a message it must refuse.
func (c *jsonrpcConn) write(out rpcResponse) {
	b, err := json.Marshal(out)
	if err != nil {
		out = rpcResponse{JSONRPC: jsonrpcVersion, ID: out.ID, Error: &rpcBody{Code: codeInternal, Message: errInternal.msg}}
		b, _ = json.Marshal(out) //nolint:errchkjson // an error body and an id that decoded as JSON always encode
	}
	if len(b) > protocol.MaxMessageBytes {
		out = rpcResponse{JSONRPC: jsonrpcVersion, ID: out.ID, Error: &rpcBody{Code: errTooBig.code, Message: errTooBig.msg}}
		b, _ = json.Marshal(out) //nolint:errchkjson // as above
	}
	b = append(b, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, _ = c.w.Write(b)
}

// readLine reads one line without its newline. It fails with errTooLong as soon
// as the line is known to be longer than maxLen, so a peer cannot make the
// plugin buffer more than that.
func readLine(br *bufio.Reader, maxLen int) ([]byte, error) {
	var buf []byte
	for {
		frag, err := br.ReadSlice('\n')
		limit := maxLen
		if err == nil {
			limit = maxLen + 1 // the newline
		}
		if len(buf)+len(frag) > limit {
			return nil, errTooLong
		}
		buf = append(buf, frag...)
		switch {
		case err == nil:
			return buf[:len(buf)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}

// serveJSONRPCListener accepts connections until ctx ends. Each connection is
// served on its own; the plugin's state is shared between them.
func (s *server) serveJSONRPCListener(ctx context.Context, l net.Listener) error {
	// A failed accept cancels this context before the wait below, so the
	// connections still open end instead of holding Serve up.
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	go func() {
		<-connCtx.Done()
		_ = l.Close()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			stopping := ctx.Err() != nil
			cancel()
			wg.Wait()
			if stopping {
				return nil // the accept failed because Serve is stopping
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A connection that sent an oversized line is closed; the plugin
			// goes on serving the others.
			_ = s.serveConn(connCtx, c, c)
		}()
	}
}
