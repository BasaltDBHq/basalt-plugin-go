package basaltplugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/basaltdbhq/basalt-plugin-go/protocol"
)

const testdata = "testdata"

// notifierPeer is the smallest complete notifier: it handles every event and
// every notice.
type notifierPeer struct{}

func (notifierPeer) Info() Info { return Info{Name: "sdk-notifier", Version: "1.0.0"} }

func (notifierPeer) ObserveFeed(_ context.Context, b FeedBatch) (int64, error) {
	return b.LastSeq(), nil
}

func (notifierPeer) DeliverApproval(context.Context, Notice) error { return nil }

// recordingNotifier keeps what it got and answers as told.
type recordingNotifier struct {
	mu      sync.Mutex
	batches []FeedBatch
	notices []Notice
	answer  func(FeedBatch) (int64, error)
	deliver error
}

func (*recordingNotifier) Info() Info { return Info{Name: "rec-notifier", Version: "1.0.0"} }

func (r *recordingNotifier) ObserveFeed(_ context.Context, b FeedBatch) (int64, error) {
	r.mu.Lock()
	r.batches = append(r.batches, b)
	r.mu.Unlock()
	if r.answer != nil {
		return r.answer(b)
	}
	return b.LastSeq(), nil
}

func (r *recordingNotifier) DeliverApproval(_ context.Context, n Notice) error {
	r.mu.Lock()
	r.notices = append(r.notices, n)
	r.mu.Unlock()
	return r.deliver
}

func (r *recordingNotifier) lastBatch(t *testing.T) FeedBatch {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.batches)
	return r.batches[len(r.batches)-1]
}

func (r *recordingNotifier) lastNotice(t *testing.T) Notice {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.notices)
	return r.notices[len(r.notices)-1]
}

type panickingDeliverer struct{}

func (panickingDeliverer) Info() Info { return Info{Name: "panics", Version: "1.0.0"} }

func (panickingDeliverer) DeliverApproval(context.Context, Notice) error { panic(marker) }

// feedOnly implements one hook.
type feedOnly struct{}

func (feedOnly) Info() Info { return Info{Name: "feed-only", Version: "1.0.0"} }

func (feedOnly) ObserveFeed(_ context.Context, b FeedBatch) (int64, error) {
	return b.LastSeq(), nil
}

func readSample(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testdata, name))
	require.NoError(t, err)
	return b
}

// compact is what a transport sends: the sample with no insignificant space.
func compact(t *testing.T, b []byte) string {
	t.Helper()
	var out bytes.Buffer
	require.NoError(t, json.Compact(&out, b))
	return out.String()
}

// TestHooksAreDeclaredFromTheInterfaces: a plugin declares exactly the hooks
// whose interfaces it implements, with the contract of each.
func TestHooksAreDeclaredFromTheInterfaces(t *testing.T) {
	for _, tc := range []struct {
		p    Plugin
		want map[string]protocol.HookDecl
	}{
		{&recordingNotifier{}, map[string]protocol.HookDecl{"feed.observe": {Contract: "feed/1"}, "approval.deliver": {Contract: "deliver/1"}}},
		{feedOnly{}, map[string]protocol.HookDecl{"feed.observe": {Contract: "feed/1"}}},
		{newFull(), map[string]protocol.HookDecl{}},
	} {
		_, c := serveStdio(t, tc.p)
		r := c.call("initialize", initParams)
		require.Nil(t, r.Error)
		var res protocol.InitializeResponse
		require.NoError(t, json.Unmarshal(r.Result, &res))
		require.Equal(t, tc.want, res.Hooks, "%T", tc.p)
	}
	// A hook the plugin does not implement is "method not found".
	_, c := serveStdio(t, feedOnly{})
	require.Equal(t, codeNoMethod, c.call("approval.deliver", `{"agent":"a","event":`+noticeEvent+`}`).Error.Code)
}

// TestWithHooksCannotDeclareAHookThePluginDoesNotImplement: declaring a hook
// the SDK has an interface for, without implementing it, is refused at once
// instead of failing every call the engine makes.
func TestWithHooksCannotDeclareAHookThePluginDoesNotImplement(t *testing.T) {
	for _, h := range []string{HookFeedObserve, HookApprovalDeliver} {
		err := Serve(context.Background(), newFull(), WithHooks(map[string]HookDecl{h: {Contract: "x/1"}}))
		require.ErrorIs(t, err, ErrHookNotImplemented, h)
	}
	err := Serve(context.Background(), feedOnly{}, WithHooks(map[string]HookDecl{
		HookFeedObserve: {Contract: ContractFeed}, HookApprovalDeliver: {Contract: ContractDeliver},
	}))
	require.ErrorIs(t, err, ErrHookNotImplemented)
}

// TestEverySampleDecodesTyped: every valid feed and notice sample of the
// contract reaches the plugin as typed values, and Raw is the event's bytes
// as the engine sent them.
func TestEverySampleDecodesTyped(t *testing.T) {
	feeds, err := filepath.Glob(filepath.Join(testdata, "valid-feed-request*.json"))
	require.NoError(t, err)
	require.Len(t, feeds, 3)
	for _, f := range feeds {
		p := &recordingNotifier{}
		_, c := serveStdio(t, p)
		params := compact(t, readSample(t, filepath.Base(f)))
		r := c.call("feed.observe", params)
		require.Nil(t, r.Error, f)
		var in struct {
			Agent  string            `json:"agent"`
			Events []json.RawMessage `json:"events"`
		}
		require.NoError(t, json.Unmarshal([]byte(params), &in))
		b := p.lastBatch(t)
		require.Equal(t, in.Agent, b.Agent)
		require.Len(t, b.Events, len(in.Events))
		for i, ev := range b.Events {
			require.Equal(t, string(in.Events[i]), string(ev.Raw()), "%s event %d", f, i)
			require.NotNil(t, ev.Detail, "%s: every sample type is known", f)
			require.NotZero(t, ev.Seq)
			require.Equal(t, in.Agent, ev.Agent)
			require.Equal(t, in.Agent, ev.Subject)
			require.Equal(t, "1.0", ev.SpecVersion)
			require.Equal(t, "urn:uuid:"+ev.InstallID, ev.Source)
		}
		require.JSONEq(t, `{"lastSeq":`+strconv.FormatInt(b.LastSeq(), 10)+`}`, string(r.Result))
	}

	// The full sample, field by field.
	p := &recordingNotifier{}
	_, c := serveStdio(t, p)
	require.Nil(t, c.call("feed.observe", compact(t, readSample(t, "valid-feed-request.json"))).Error)
	evs := p.lastBatch(t).Events
	require.Equal(t, WriteApplied{
		ChangeID: "0193a1b3-0000-7000-8000-000000000001", RunID: "0193a1b2-7c4d-7e8f-9a0b-1c2d3e4f5a6b",
		TaskID: "0193a1b2-7c4d-7e8f-9a0b-1c2d3e4f5a00", Op: OpUpdate, Table: "public.cases", Rows: 1,
	}, evs[0].Detail)
	require.Equal(t, TypeWriteApplied, evs[0].Type)
	require.Equal(t, time.Date(2026, 11, 19, 6, 2, 11, 513958000, time.UTC), evs[0].Time.UTC())
	require.Equal(t, int64(4211), evs[0].Seq)
	require.Equal(t, WriteRefused{
		ChangeID: "0193a1b3-0000-7000-8000-000000000002", RunID: "0193a1b2-7c4d-7e8f-9a0b-1c2d3e4f5a6b",
		Op: OpDelete, ReasonCode: "ROW_LIMIT_EXCEEDED",
	}, evs[1].Detail)
	held, ok := evs[2].Detail.(WriteHeld)
	require.True(t, ok)
	require.Equal(t, int64(27), held.Rows)
	require.Equal(t, "0193a1b3-1111-7000-8000-000000000001", held.ApprovalID)
	require.Equal(t, time.Date(2026, 11, 20, 6, 3, 2, 120000000, time.UTC), held.ExpiresAt.UTC())
	require.Equal(t, ApprovalSigned{ChangeID: "0193a1b3-0000-7000-8000-000000000003", ApprovalID: "0193a1b3-1111-7000-8000-000000000001"}, evs[3].Detail)
	require.Equal(t, RevertPrepared{RevertID: "0193a1b3-2222-7000-8000-000000000001"}, evs[5].Detail)
	require.Equal(t, RevertApplied{ChangeID: "0193a1b3-0000-7000-8000-000000000005", RevertID: "0193a1b3-2222-7000-8000-000000000001"}, evs[6].Detail)

	// The skipped marker has no time; the batch goes on with revert events.
	require.Nil(t, c.call("feed.observe", compact(t, readSample(t, "valid-feed-request-skipped.json"))).Error)
	evs = p.lastBatch(t).Events
	require.Equal(t, FeedSkipped{AfterSeq: 120}, evs[0].Detail)
	require.Equal(t, TypeFeedSkipped, evs[0].Type)
	require.True(t, evs[0].Time.IsZero())
	require.Equal(t, int64(9800), evs[0].Seq)
	require.Equal(t, RevertSigned{ChangeID: "0193a1b3-0000-7000-8000-000000000001", RevertID: "0193a1b3-2222-7000-8000-000000000001"}, evs[1].Detail)
	require.Equal(t, RevertRejected{RevertID: "0193a1b3-2222-7000-8000-000000000002"}, evs[2].Detail)

	notices, err := filepath.Glob(filepath.Join(testdata, "valid-deliver-request*.json"))
	require.NoError(t, err)
	require.Len(t, notices, 4)
	for _, f := range notices {
		p := &recordingNotifier{}
		_, c := serveStdio(t, p)
		params := compact(t, readSample(t, filepath.Base(f)))
		r := c.call("approval.deliver", params)
		require.Nil(t, r.Error, f)
		require.JSONEq(t, `{}`, string(r.Result))
		var in struct {
			Event json.RawMessage `json:"event"`
		}
		require.NoError(t, json.Unmarshal([]byte(params), &in))
		n := p.lastNotice(t)
		require.Equal(t, string(in.Event), string(n.Raw()), f)
		require.Contains(t, []NoticeKind{KindApprovalRequested, KindRevertRequested, KindLinkReplaced}, n.Kind, f)
		require.Contains(t, []string{
			InstructionWriteAnywhere, InstructionWriteEngineComputer, InstructionWriteNoLink,
			InstructionRevertAnywhere, InstructionRevertEngineComputer, InstructionRevertNoLink,
		}, n.Instruction, f)
		require.NotEmpty(t, n.Headline)
		require.False(t, n.ExpiresAt.IsZero())
		require.NotEqual(t, n.ApprovalID == "", n.RevertID == "", "%s: exactly one id", f)
		require.Equal(t, n.Link == "", n.LinkOpens == "", f)
	}

	// Only check's samples are samples.
	p = &recordingNotifier{}
	_, c = serveStdio(t, p)
	require.Nil(t, c.call("approval.deliver", compact(t, readSample(t, "valid-deliver-request-check-sample.json"))).Error)
	require.True(t, p.lastNotice(t).IsCheckSample())
	require.Nil(t, c.call("feed.observe", compact(t, readSample(t, "valid-feed-request-check-sample.json"))).Error)
	require.True(t, p.lastBatch(t).Events[0].IsCheckSample())
	require.Nil(t, c.call("feed.observe", compact(t, readSample(t, "valid-feed-request.json"))).Error)
	require.False(t, p.lastBatch(t).Events[0].IsCheckSample())

	p = &recordingNotifier{}
	_, c = serveStdio(t, p)
	require.Nil(t, c.call("approval.deliver", compact(t, readSample(t, "valid-deliver-request-revert-portal.json"))).Error)
	n := p.lastNotice(t)
	require.False(t, n.IsCheckSample())
	require.Equal(t, TypeRevertRequested, n.Type)
	require.Equal(t, KindRevertRequested, n.Kind)
	require.Equal(t, "product_a", n.Agent)
	require.Equal(t, "0193a1b3-2222-7000-8000-000000000001", n.RevertID)
	require.Equal(t, LinkOpensAnywhere, n.LinkOpens)
	require.True(t, strings.HasPrefix(n.Link, "https://portal.basaltvault.app/reverts/"))
	require.Equal(t, InstructionRevertAnywhere, n.Instruction)
	require.Equal(t, time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC), n.ExpiresAt.UTC())
}

// TestRawIsTheBytesAsSent: odd spacing, key order, an unknown field and
// characters JSON may escape all survive into Raw, on every transport.
func TestRawIsTheBytesAsSent(t *testing.T) {
	odd := `{ "data" : {"seq": 3, "table": "a<b&c>", "zz": [1, 2]}, "type":"dev.basaltdb.write.applied",` +
		` "specversion" : "1.0", "id":"i:3", "source":"urn:uuid:i", "subject":"product_a", "x-ext": "é"}`
	notice := `{ "type": "dev.basaltdb.approval.requested", "data": {"kind": "APPROVAL_REQUESTED", "headline": "a<b", "later": null}, "id": "n" }`

	t.Run("stdio", func(t *testing.T) {
		p := &recordingNotifier{}
		_, c := serveStdio(t, p)
		require.Nil(t, c.call("feed.observe", `{"agent":"product_a","events":[`+odd+`]}`).Error)
		require.Equal(t, odd, string(p.lastBatch(t).Events[0].Raw()))
		require.Nil(t, c.call("approval.deliver", `{"agent":"product_a","event":`+notice+`}`).Error)
		require.Equal(t, notice, string(p.lastNotice(t).Raw()))
	})
	t.Run("connectrpc", func(t *testing.T) {
		p := &recordingNotifier{}
		_, path := serveSocket(t, p, WithTransport(ConnectRPC))
		tr := dialT(t, dialConnect, path)
		// The engine sends the batch compacted, as on JSON-RPC.
		var res protocol.FeedObserveResponse
		require.NoError(t, tr.Call(ctxT(t), protocol.MethodFeedObserve,
			protocol.FeedObserveRequest{Agent: "product_a", Events: json.RawMessage("[" + odd + "]")}, &res))
		require.Equal(t, int64(3), res.LastSeq)
		require.Equal(t, compact(t, []byte(odd)), string(p.lastBatch(t).Events[0].Raw()))
		require.Contains(t, string(p.lastBatch(t).Events[0].Raw()), "a<b&c>")
	})
}

// TestUnknownTypeAndFieldsReachThePlugin: an event of a type this SDK does
// not know arrives with no Detail and its Seq, which the plugin may answer;
// unknown fields are ignored; a notice of an unknown type still arrives.
func TestUnknownTypeAndFieldsReachThePlugin(t *testing.T) {
	p := &recordingNotifier{}
	_, c := serveStdio(t, p)
	unknown := `{"specversion":"1.0","id":"x:9","source":"urn:uuid:x","type":"dev.basaltdb.later.thing","subject":"product_a",` +
		`"data":{"installId":"x","seq":9,"agent":"product_a","newField":{"a":1}}}`
	known := strings.Replace(feedEvent(4), `"rows":1`, `"rows":1,"laterField":"v"`, 1)
	r := c.call("feed.observe", `{"agent":"product_a","events":[`+known+`,`+unknown+`],"laterParam":1}`)
	require.Nil(t, r.Error)
	require.JSONEq(t, `{"lastSeq":9}`, string(r.Result))
	b := p.lastBatch(t)
	require.Equal(t, int64(1), b.Events[0].Detail.(WriteApplied).Rows)
	require.Nil(t, b.Events[1].Detail)
	require.Equal(t, int64(9), b.Events[1].Seq)
	require.Equal(t, "dev.basaltdb.later.thing", b.Events[1].Type)

	later := `{"specversion":"1.0","id":"n2","type":"dev.basaltdb.later.notice","data":{"kind":"LATER_KIND","agent":"product_a"}}`
	require.Nil(t, c.call("approval.deliver", `{"agent":"product_a","event":`+later+`}`).Error)
	require.Equal(t, NoticeKind("LATER_KIND"), p.lastNotice(t).Kind)

	// A later data field may share a name with an envelope attribute, in any
	// case and of any type: it is still an unknown field, and ignored.
	for _, extra := range []string{`"time":5`, `"subject":{}`, `"id":7`, `"Source":[1]`, `"data":"x"`, `"specversion":2`} {
		notice := strings.Replace(noticeEvent, `"kind":"APPROVAL_REQUESTED"`, `"kind":"APPROVAL_REQUESTED",`+extra, 1)
		r := c.call("approval.deliver", `{"agent":"product_a","event":`+notice+`}`)
		require.Nil(t, r.Error, extra)
		n := p.lastNotice(t)
		require.Equal(t, "0193a1b4-0000-7000-8000-000000000001", n.ID, extra)
		require.Equal(t, "product_a", n.Subject, extra)
		require.Equal(t, KindApprovalRequested, n.Kind, extra)
	}
	// So may a feed event's data.
	ev := strings.Replace(feedEvent(5), `"rows":1`, `"rows":1,"time":5,"subject":{},"id":7`, 1)
	require.Nil(t, c.call("feed.observe", `{"agent":"product_a","events":[`+ev+`]}`).Error)
	require.Equal(t, "0193a0f2-5b7e-7c41-9d2a-6e1f00c0ffee:5", p.lastBatch(t).Events[0].ID)
}

// TestObserveFeedAnswerIsHeldToTheContract: a lastSeq that is not a seq of the
// batch, or an error, is never sent as an answer, and the plugin's error
// text never reaches the engine.
func TestObserveFeedAnswerIsHeldToTheContract(t *testing.T) {
	batch := `{"agent":"product_a","events":` + string(feedBatch(feedEvent(10), feedEvent(12))) + `}`
	for _, bad := range []int64{0, 11, 13, -1} {
		p := &recordingNotifier{answer: func(FeedBatch) (int64, error) { return bad, nil }}
		_, c := serveStdio(t, p)
		r := c.call("feed.observe", batch)
		require.Nil(t, r.Result, "lastSeq %d", bad)
		require.Equal(t, codeRefused, r.Error.Code)
	}
	p := &recordingNotifier{answer: func(FeedBatch) (int64, error) { return 10, nil }}
	_, c := serveStdio(t, p)
	require.JSONEq(t, `{"lastSeq":10}`, string(c.call("feed.observe", batch).Result), "a prefix of the batch is a good answer")

	p = &recordingNotifier{answer: func(FeedBatch) (int64, error) { return 12, errors.New(marker) }}
	_, c = serveStdio(t, p)
	r := c.call("feed.observe", batch)
	require.Nil(t, r.Result)
	require.NotContains(t, r.Error.Message, marker)

	p = &recordingNotifier{deliver: errors.New(marker)}
	_, c = serveStdio(t, p)
	r = c.call("approval.deliver", `{"agent":"product_a","event":`+noticeEvent+`}`)
	require.NotNil(t, r.Error)
	require.NotContains(t, r.Error.Message, marker)

	_, c = serveStdio(t, panickingDeliverer{})
	r = c.call("approval.deliver", `{"agent":"product_a","event":`+noticeEvent+`}`)
	require.NotNil(t, r.Error, "a panic in DeliverApproval fails the call")
	require.Nil(t, c.call("health", `{}`).Error, "and the plugin still serves")

	// A handler that panics fails the call; the plugin goes on serving.
	p = &recordingNotifier{answer: func(FeedBatch) (int64, error) { panic(marker) }}
	_, c = serveStdio(t, p)
	r = c.call("feed.observe", batch)
	require.NotNil(t, r.Error)
	require.NotContains(t, r.Error.Message, marker)
	p.answer = nil
	require.JSONEq(t, `{"lastSeq":12}`, string(c.call("feed.observe", batch).Result))

	// A batch that is not one: refused before the plugin sees it.
	p = &recordingNotifier{}
	_, c = serveStdio(t, p)
	for _, params := range []string{
		`{"agent":"a","events":[]}`, `{"agent":"a","events":{}}`, `{"agent":"a","events":[{"data":{}}]}`,
		`{"agent":"a","events":[{"data":[]}]}`, `{"agent":"a","events":[{"type":"dev.basaltdb.write.applied","data":{"seq":1,"rows":"x"}}]}`,
	} {
		require.Equal(t, codeInvalidParams, c.call("feed.observe", params).Error.Code, params)
	}
	require.Equal(t, codeInvalidParams, c.call("approval.deliver", `{"agent":"a","event":"x"}`).Error.Code)
	require.Empty(t, p.batches)
	require.Empty(t, p.notices)
}

// TestConnectHookCallsHaveFourSlots: over ConnectRPC at most four hook calls
// run at once, a fifth waits for a slot until its deadline, and health is
// still answered meanwhile.
func TestConnectHookCallsHaveFourSlots(t *testing.T) {
	var running, peak atomic.Int32
	release := make(chan struct{})
	p := &recordingNotifier{answer: func(b FeedBatch) (int64, error) {
		n := running.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		return b.LastSeq(), nil
	}}
	_, path := serveSocket(t, p, WithTransport(ConnectRPC))
	tr := dialT(t, dialConnect, path)
	var init protocol.InitializeResponse
	require.NoError(t, tr.Call(ctxT(t), protocol.MethodInitialize, protocol.InitializeRequest{
		Protocols: []string{protocol.Version1}, Limits: protocol.Limits{TimeoutMs: 400},
	}, &init))

	var wg sync.WaitGroup
	errs := make(chan error, MaxInFlight+1)
	for i := range MaxInFlight + 1 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var res protocol.FeedObserveResponse
			errs <- tr.Call(ctxT(t), protocol.MethodFeedObserve,
				protocol.FeedObserveRequest{Agent: "a", Events: feedBatch(feedEvent(int64(i + 1)))}, &res)
		}()
	}
	require.Eventually(t, func() bool { return running.Load() == MaxInFlight }, wait, 5*time.Millisecond)
	var h protocol.HealthResponse
	require.NoError(t, tr.Call(ctxT(t), protocol.MethodHealth, protocol.HealthRequest{}, &h), "health beside four hook calls")
	// The fifth waits for a slot until its deadline and is answered deadline exceeded.
	select {
	case err := <-errs:
		require.Error(t, err)
	case <-time.After(wait):
		t.Fatal("the fifth call was neither run nor answered")
	}

	// Another connection (another entry reaching the same server) has slots
	// of its own: its call runs while the first connection's four are taken.
	other := dialT(t, dialConnect, path)
	otherDone := make(chan error, 1)
	go func() {
		var res protocol.FeedObserveResponse
		otherDone <- other.Call(ctxT(t), protocol.MethodFeedObserve,
			protocol.FeedObserveRequest{Agent: "a", Events: feedBatch(feedEvent(99))}, &res)
	}()
	require.Eventually(t, func() bool { return running.Load() == MaxInFlight+1 }, wait, 5*time.Millisecond,
		"the other connection's call runs beside the first's four")

	close(release)
	wg.Wait()
	close(errs)
	<-otherDone // it ran, which is the point; its answer may be past its deadline
	require.EqualValues(t, MaxInFlight+1, peak.Load())
}

// TestConnectCallsPastTheWaitingCapAreAnsweredBusy: with four calls running
// and eight waiting on one connection, the next is answered at once, without
// waiting for its deadline.
func TestConnectCallsPastTheWaitingCapAreAnsweredBusy(t *testing.T) {
	var running atomic.Int32
	release := make(chan struct{})
	p := &recordingNotifier{answer: func(b FeedBatch) (int64, error) {
		running.Add(1)
		<-release
		return b.LastSeq(), nil
	}}
	_, path := serveSocket(t, p, WithTransport(ConnectRPC))
	tr := dialT(t, dialConnect, path)
	var init protocol.InitializeResponse
	require.NoError(t, tr.Call(ctxT(t), protocol.MethodInitialize, protocol.InitializeRequest{
		Protocols: []string{protocol.Version1}, Limits: protocol.Limits{TimeoutMs: 10000},
	}, &init))

	call := func(seq int64) error {
		var res protocol.FeedObserveResponse
		return tr.Call(ctxT(t), protocol.MethodFeedObserve,
			protocol.FeedObserveRequest{Agent: "a", Events: feedBatch(feedEvent(seq))}, &res)
	}
	var wg sync.WaitGroup
	for i := range MaxInFlight + waitingCalls {
		wg.Add(1)
		go func() { defer wg.Done(); _ = call(int64(i + 1)) }()
	}
	t.Cleanup(func() { close(release); wg.Wait() })
	require.Eventually(t, func() bool { return running.Load() == MaxInFlight }, wait, 5*time.Millisecond)
	// Give the waiting calls time to be admitted; then one more is turned away.
	require.Eventually(t, func() bool {
		start := time.Now()
		err := call(1000)
		return connectrpc.CodeOf(err) == connectrpc.CodeResourceExhausted && time.Since(start) < time.Second
	}, wait, 50*time.Millisecond)
}

// TestNotifierInAChildProcessOnEveryTransport: both hooks reach a notifier
// built from the SDK and run as its own process, on every transport, and an
// event of a type or with a field it does not know does not break the call.
func TestNotifierInAChildProcessOnEveryTransport(t *testing.T) {
	later := `{"specversion":"1.0","id":"x:9","source":"urn:uuid:x","type":"dev.basaltdb.later.thing",` +
		`"subject":"product_a","datacontenttype":"application/json","newattribute":"v",` +
		`"data":{"installId":"x","seq":9,"agent":"product_a","newField":{"a":[1,2]}}}`
	const big = 1<<53 + 1 // a seq a float64 cannot hold

	t.Run("jsonrpc-stdio", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], peerFlag, "notifier")
		cmd.Env = []string{}
		in, err := cmd.StdinPipe()
		require.NoError(t, err)
		out, err := cmd.StdoutPipe()
		require.NoError(t, err)
		require.NoError(t, cmd.Start())
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
		c := &stdioClient{t: t, w: in, r: bufio.NewReaderSize(out, MaxMessageBytes+4096)}
		r := c.call("feed.observe", `{"agent":"product_a","events":`+string(feedBatch(feedEvent(5), later, feedEvent(big)))+`}`)
		require.Nil(t, r.Error)
		require.JSONEq(t, `{"lastSeq":`+strconv.FormatInt(big, 10)+`}`, string(r.Result))
		r = c.call("approval.deliver", `{"agent":"product_a","event":`+noticeEvent+`}`)
		require.Nil(t, r.Error)
		require.JSONEq(t, `{}`, string(r.Result))
	})
	for _, tc := range []struct {
		name, mode string
		dial       func(context.Context, string) (wire, error)
	}{
		{"jsonrpc-unix", "notifier", dialJSONRPC},
		{"connectrpc", "notifier-connect", dialConnect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := socketPath(t)
			peerProc(t, tc.mode, SocketEnv+"="+path)
			waitSocket(t, path)
			tr := dialT(t, tc.dial, path)
			var res protocol.FeedObserveResponse
			require.NoError(t, tr.Call(ctxT(t), protocol.MethodFeedObserve,
				protocol.FeedObserveRequest{Agent: "product_a", Events: feedBatch(feedEvent(5), later, feedEvent(big))}, &res))
			require.Equal(t, int64(big), res.LastSeq)
			var empty map[string]any
			require.NoError(t, tr.Call(ctxT(t), protocol.MethodApprovalDeliver,
				protocol.ApprovalDeliverRequest{Agent: "product_a", Event: json.RawMessage(noticeEvent)}, &empty))
			require.Empty(t, empty)
		})
	}
}
