package protocol

import "encoding/json"

// The notifier hooks' methods, as the JSON-RPC method names. ConnectRPC maps
// them to PluginService/FeedObserve and ApprovalDeliver.
const (
	MethodFeedObserve     = "feed.observe"
	MethodApprovalDeliver = "approval.deliver"
)

// MaxFeedEvents is the most events one feed.observe call carries.
const MaxFeedEvents = 100

// FeedObserveRequest is one feed.observe call (feed/1): one agent's events,
// in ascending seq. Events is the CloudEvents JSON batch, kept as bytes so the
// events reach the plugin as the engine built them, on every transport.
type FeedObserveRequest struct {
	Agent  string          `json:"agent"`
	Events json.RawMessage `json:"events"`
}

// FeedObserveResponse is the plugin's answer: the seq of the last event it
// handled, which must be the data.seq of one event of the batch.
type FeedObserveResponse struct {
	LastSeq int64 `json:"lastSeq"`
}

// ApprovalDeliverRequest is one approval.deliver call (deliver/1): one
// notice, a CloudEvent in its JSON form, kept as bytes like a feed batch.
type ApprovalDeliverRequest struct {
	Agent string          `json:"agent"`
	Event json.RawMessage `json:"event"`
}

// ApprovalDeliverResponse is empty.
type ApprovalDeliverResponse struct{}
