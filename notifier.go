package basaltplugin

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/basaltdbhq/basalt-plugin-go/protocol"
)

// FeedObserver is implemented by a plugin that observes the feed
// (feed.observe, contract feed/1): what an agent did to data and what
// happened to its approvals and reverts.
//
// ObserveFeed gets one agent's events, in order, and returns the Seq of the
// last event it handled. Return batch.LastSeq() once you handled them all,
// or the Seq of the last one you handled, and the engine sends the rest
// again. The SDK answers only a Seq that is one of the batch's: any other,
// or an error, is a failed call, and the engine sends the same events again
// after a backoff. Events arrive at least once: dedup by Source and ID.
//
// Skip an event whose Detail is nil (a type this SDK does not know) or that
// you do not need, and still count it as handled.
type FeedObserver interface {
	ObserveFeed(ctx context.Context, batch FeedBatch) (lastSeq int64, err error)
}

// ApprovalDeliverer is implemented by a plugin that delivers approval
// notices (approval.deliver, contract deliver/1) to the person: a held
// write, a prepared revert or a replaced link.
//
// DeliverApproval returns nil once the notice is delivered. An error is a
// failed try: the engine tries a notice up to four times over about a
// minute, then drops it. It never sends one past its ExpiresAt. Skip a
// notice whose Kind you do not know.
type ApprovalDeliverer interface {
	DeliverApproval(ctx context.Context, notice Notice) error
}

var (
	// errNotHandled: the plugin's handler returned an error. Its text stays
	// in the plugin, as for Initializer.
	errNotHandled = &rpcError{codeRefused, "the plugin did not handle the call"}
	// errSeqNotInBatch: ObserveFeed answered a seq the contract forbids.
	errSeqNotInBatch = &rpcError{codeRefused, "the plugin answered a lastSeq that is not a seq of the batch"}
)

func init() {
	hookSpecs = append(hookSpecs,
		hookSpec{
			name: HookFeedObserve,
			declare: func(p Plugin) (HookDecl, bool) {
				_, ok := p.(FeedObserver)
				return HookDecl{Contract: ContractFeed}, ok
			},
			methods: map[string]func(Plugin) methodFunc{
				protocol.MethodFeedObserve: func(p Plugin) methodFunc { h, _ := p.(FeedObserver); return observeFeed(h) },
			},
		},
		hookSpec{
			name: HookApprovalDeliver,
			declare: func(p Plugin) (HookDecl, bool) {
				_, ok := p.(ApprovalDeliverer)
				return HookDecl{Contract: ContractDeliver}, ok
			},
			methods: map[string]func(Plugin) methodFunc{
				protocol.MethodApprovalDeliver: func(p Plugin) methodFunc { h, _ := p.(ApprovalDeliverer); return deliverApproval(h) },
			},
		},
	)
}

// decodeFeedBatch decodes feed.observe params: {agent, events}.
func decodeFeedBatch(params json.RawMessage) (FeedBatch, *rpcError) {
	var req struct {
		Agent  string            `json:"agent"`
		Events []json.RawMessage `json:"events"`
	}
	if json.Unmarshal(params, &req) != nil || len(req.Events) == 0 {
		return FeedBatch{}, errBadParams
	}
	b := FeedBatch{Agent: req.Agent, Events: make([]FeedEvent, len(req.Events))}
	for i, raw := range req.Events {
		ev, err := decodeFeedEvent(raw)
		if err != nil {
			return FeedBatch{}, errBadParams
		}
		b.Events[i] = ev
	}
	return b, nil
}

func observeFeed(h FeedObserver) methodFunc {
	return func(ctx context.Context, params json.RawMessage) (any, *rpcError) {
		batch, e := decodeFeedBatch(params)
		if e != nil {
			return nil, e
		}
		seqs := make([]int64, len(batch.Events))
		for i, ev := range batch.Events {
			seqs[i] = ev.Seq
		}
		var last int64
		err := recovered(func() (err error) {
			last, err = h.ObserveFeed(ctx, batch)
			return err
		})
		switch {
		case ctx.Err() != nil:
			return nil, errDeadline
		case err != nil:
			return nil, errNotHandled
		case !slices.Contains(seqs, last):
			return nil, errSeqNotInBatch
		}
		return protocol.FeedObserveResponse{LastSeq: last}, nil
	}
}

func deliverApproval(h ApprovalDeliverer) methodFunc {
	return func(ctx context.Context, params json.RawMessage) (any, *rpcError) {
		var req struct {
			Event json.RawMessage `json:"event"`
		}
		if json.Unmarshal(params, &req) != nil {
			return nil, errBadParams
		}
		n, err := decodeNotice(req.Event)
		if err != nil {
			return nil, errBadParams
		}
		err = recovered(func() error { return h.DeliverApproval(ctx, n) })
		switch {
		case ctx.Err() != nil:
			return nil, errDeadline
		case err != nil:
			return nil, errNotHandled
		}
		return protocol.ApprovalDeliverResponse{}, nil
	}
}

// errPanicked stands for a handler that panicked. Its value stays in the
// plugin, like a handler's error.
var errPanicked = errors.New("basaltplugin: the handler panicked")

// recovered runs a hook handler and turns a panic into an error, so one bad
// event fails its call instead of ending the plugin.
func recovered(f func() error) (err error) {
	defer func() {
		if recover() != nil {
			err = errPanicked
		}
	}()
	return f()
}
