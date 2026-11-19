package basaltplugin

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/basaltdbhq/basalt-plugin-go/protocol"
)

// Deadlines. The engine sends the entry's timeoutMs in initialize; until then a
// call gets defaultTimeout.
const (
	defaultTimeout = 2 * time.Second
	minTimeoutMs   = 50
	maxTimeoutMs   = 60000
)

// versions are the protocol versions this SDK speaks, newest first.
var versions = []string{protocol.Version1}

// JSON-RPC error codes, which the ConnectRPC side maps to its own codes. The
// messages are fixed strings: no payload and no plugin error text is ever put
// in one.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeNoMethod       = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeRefused        = -32000 // the plugin refuses this call
	codeDeadline       = -32001
	codeBusy           = -32002 // too many calls wait for a slot
)

type rpcError struct {
	code int
	msg  string
}

func (e *rpcError) Error() string { return e.msg }

var (
	errNoMethod      = &rpcError{codeNoMethod, "method not found"}
	errBadParams     = &rpcError{codeInvalidParams, "invalid params"}
	errInternal      = &rpcError{codeInternal, "internal error"}
	errDeadline      = &rpcError{codeDeadline, "deadline exceeded"}
	errBusy          = &rpcError{codeBusy, "too many calls waiting"}
	errNoProtocol    = &rpcError{codeRefused, "no protocol version in common; this plugin speaks " + protocol.Version1}
	errHookNotServed = &rpcError{codeRefused, "the engine lists a hook this plugin does not implement"}
	errInitFailed    = &rpcError{codeRefused, "the plugin refused to start"}
	errTooBig        = &rpcError{codeInternal, "reply over the size cap"}
)

// methodFunc serves one protocol method: it decodes its own params from the
// JSON-RPC form and returns a value that is encoded as the result.
type methodFunc func(ctx context.Context, params json.RawMessage) (any, *rpcError)

// hookSpec is how the SDK learns of one hook. A hook's own work adds one entry
// to hookSpecs; nothing else in the SDK changes, and no existing plugin does.
// To add a hook, in this package:
//
//  1. Define its optional interface (for example WireDetector) in its own file,
//     with the typed payloads its contract defines.
//  2. Add a hookSpec to hookSpecs: name is the hook's lowercase dotted name,
//     declare returns the plugin's declaration when it implements the
//     interface (a type assertion on p), and methods maps each of the hook's
//     JSON-RPC method names to a function that decodes the params, calls the
//     interface and returns the result.
//  3. Add the hook's methods to plugin.proto, regenerate, and implement them on
//     connectService in connect.go by calling the same functions.
//
// The SDK then declares the hook in initialize for plugins that implement it,
// serves its methods only for plugins that declared it, and leaves every other
// plugin as it was.
type hookSpec struct {
	name    string
	declare func(p Plugin) (HookDecl, bool)
	methods map[string]func(p Plugin) methodFunc
}

// hookSpecs are the hooks the SDK has interfaces for. notifier.go adds the
// notifier hooks.
var hookSpecs []hookSpec

// server is the transport-neutral part of a plugin: the three platform methods
// and the hook methods. A transport decodes a call, runs it here, and encodes
// the answer.
type server struct {
	p       Plugin
	info    Info
	hooks   map[string]HookDecl
	methods map[string]methodFunc

	initMu    sync.Mutex
	timeoutNs atomic.Int64

	// connectSlots are the hook call slots of a ConnectRPC request that
	// reached the server without a connection's own (a handler mounted
	// outside serveConnect). serveConnect gives each connection its own.
	connectSlots *hookSlots

	stop context.CancelFunc

	stopMu  sync.Mutex
	stopErr error
}

func newServer(p Plugin, cfg *config) (*server, error) {
	if p == nil {
		return nil, ErrBadPlugin
	}
	info := p.Info()
	if info.Name == "" || info.Version == "" {
		return nil, ErrBadPlugin
	}
	s := &server{
		p: p, info: info, hooks: map[string]HookDecl{}, methods: map[string]methodFunc{},
		connectSlots: newHookSlots(),
	}
	s.timeoutNs.Store(int64(defaultTimeout))
	for _, h := range hookSpecs {
		decl, ok := h.declare(p)
		if !ok {
			if _, given := cfg.hooks[h.name]; cfg.hooksSet && given {
				return nil, ErrHookNotImplemented
			}
			continue
		}
		// WithHooks names exactly the hooks served: a hook it leaves out
		// gets no methods, so its calls answer "method not found".
		if cfg.hooksSet {
			if _, given := cfg.hooks[h.name]; !given {
				continue
			}
		}
		s.hooks[h.name] = decl
		for name, m := range h.methods {
			s.methods[name] = m(p)
		}
	}
	if cfg.hooksSet {
		s.hooks = maps.Clone(cfg.hooks)
		if s.hooks == nil {
			s.hooks = map[string]HookDecl{}
		}
	}
	maps.Copy(s.methods, cfg.extra)
	return s, nil
}

// callContext bounds one call by the entry's timeout.
func (s *server) callContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, time.Duration(s.timeoutNs.Load()))
}

// dispatch runs one call given as JSON-RPC params.
func (s *server) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case protocol.MethodInitialize:
		var req protocol.InitializeRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, errBadParams
		}
		res, e := s.boundedInitialize(ctx, req)
		if e != nil {
			return nil, e
		}
		return res, nil
	case protocol.MethodHealth:
		return s.boundedHealth(ctx), nil
	case protocol.MethodShutdown:
		s.boundedShutdown(ctx)
		return protocol.ShutdownResponse{}, nil
	}
	m, ok := s.methods[method]
	if !ok {
		return nil, errNoMethod
	}
	res, e := m(ctx, params)
	if e == nil && ctx.Err() != nil {
		return nil, errDeadline
	}
	return res, e
}

// within runs f and waits for it or for ctx, whichever ends first, so a plugin
// handler that ignores its context cannot hold the call past its deadline. When
// ctx ends first the handler is left running and its result is dropped.
func within[T any](ctx context.Context, f func() T) (T, bool) {
	done := make(chan T, 1)
	go func() { done <- f() }()
	select {
	case v := <-done:
		return v, true
	case <-ctx.Done():
		var zero T
		return zero, false
	}
}

// The bounded forms of the three platform methods are what every transport
// calls: a plugin handler that never returns ends at the call's deadline.

// boundedInitialize answers deadline exceeded when the plugin's Initialize is late.
func (s *server) boundedInitialize(ctx context.Context, req protocol.InitializeRequest) (protocol.InitializeResponse, *rpcError) {
	type out struct {
		res protocol.InitializeResponse
		e   *rpcError
	}
	o, ok := within(ctx, func() out {
		res, e := s.initialize(ctx, req)
		return out{res, e}
	})
	if !ok {
		return protocol.InitializeResponse{}, errDeadline
	}
	return o.res, o.e
}

// boundedHealth reports a health check that is late as not ready.
func (s *server) boundedHealth(ctx context.Context) protocol.HealthResponse {
	res, ok := within(ctx, func() protocol.HealthResponse { return s.health(ctx) })
	if !ok {
		return protocol.HealthResponse{Status: protocol.HealthNotReady}
	}
	return res
}

// boundedShutdown abandons a plugin Shutdown that is late: the engine is
// stopping the plugin either way, so the call still answers.
func (s *server) boundedShutdown(ctx context.Context) {
	within(ctx, func() struct{} {
		s.shutdown(ctx)
		return struct{}{}
	})
}

// initialize negotiates the protocol, checks the hooks the engine will call and
// answers with the plugin's identity.
func (s *server) initialize(ctx context.Context, req protocol.InitializeRequest) (protocol.InitializeResponse, *rpcError) {
	var res protocol.InitializeResponse
	chosen := ""
	for _, v := range versions {
		if slices.Contains(req.Protocols, v) {
			chosen = v
			break
		}
	}
	if chosen == "" {
		return res, errNoProtocol
	}
	for _, h := range req.Hooks {
		if _, ok := s.hooks[h]; !ok {
			return res, errHookNotServed
		}
	}
	timeoutMs := req.Limits.TimeoutMs
	if timeoutMs != 0 && (timeoutMs < minTimeoutMs || timeoutMs > maxTimeoutMs) {
		return res, errBadParams
	}
	if in, ok := s.p.(Initializer); ok {
		s.initMu.Lock()
		err := in.Initialize(ctx, Init{
			Protocol:  chosen,
			Hooks:     slices.Clone(req.Hooks),
			Config:    json.RawMessage(slices.Clone(configOf(req.Config))),
			TimeoutMs: timeoutMs,
		})
		s.initMu.Unlock()
		if err != nil {
			return res, errInitFailed
		}
	}
	if ctx.Err() != nil {
		return res, errDeadline // late: the engine was already told so
	}
	if timeoutMs != 0 {
		s.timeoutNs.Store(int64(time.Duration(timeoutMs) * time.Millisecond))
	}
	return protocol.InitializeResponse{
		Protocol: chosen,
		Name:     s.info.Name,
		Version:  s.info.Version,
		Hooks:    maps.Clone(s.hooks),
		Model:    s.info.Model,
	}, nil
}

func configOf(r protocol.RawObject) []byte {
	if len(r) == 0 {
		return []byte("{}")
	}
	return r
}

func (s *server) health(ctx context.Context) protocol.HealthResponse {
	if h, ok := s.p.(HealthChecker); ok && h.Health(ctx) != nil {
		return protocol.HealthResponse{Status: protocol.HealthNotReady}
	}
	return protocol.HealthResponse{Status: protocol.HealthReady}
}

// shutdown runs the plugin's own shutdown. It never fails the call: the engine
// is stopping the plugin either way, and the error is kept for Serve.
func (s *server) shutdown(ctx context.Context) {
	if sd, ok := s.p.(Shutdowner); ok {
		if err := sd.Shutdown(ctx); err != nil {
			s.stopMu.Lock()
			s.stopErr = err
			s.stopMu.Unlock()
		}
	}
}

// requestStop ends Serve. A transport calls it once the shutdown reply is on
// the wire (or handed to the HTTP server).
func (s *server) requestStop() { s.stop() }

func (s *server) shutdownError() error {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	return s.stopErr
}
