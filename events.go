package basaltplugin

import (
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// The hooks the SDK has interfaces for, and the message contract of each.
const (
	HookFeedObserve     = "feed.observe"
	HookApprovalDeliver = "approval.deliver"

	ContractFeed    = "feed/1"
	ContractDeliver = "deliver/1"
)

// The CloudEvents type of every event the engine sends. A feed event's type
// names what happened to the agent's data or its approvals; a notice's type
// is one no feed event uses, so the two are never mistaken for each other.
const (
	TypeWriteApplied   = "dev.basaltdb.write.applied"
	TypeWriteRefused   = "dev.basaltdb.write.refused"
	TypeWriteHeld      = "dev.basaltdb.write.held"
	TypeApprovalSigned = "dev.basaltdb.approval.signed"
	TypeRevertPrepared = "dev.basaltdb.revert.prepared"
	TypeRevertSigned   = "dev.basaltdb.revert.signed"
	TypeRevertApplied  = "dev.basaltdb.revert.applied"
	TypeRevertRejected = "dev.basaltdb.revert.rejected"
	TypeFeedSkipped    = "dev.basaltdb.feed.skipped"

	TypeApprovalRequested = "dev.basaltdb.approval.requested"
	TypeRevertRequested   = "dev.basaltdb.revert.requested"
	TypeLinkReplaced      = "dev.basaltdb.link.replaced"
)

// CheckInstallID is the install id of the samples basalt plugin check sends.
// No install has it, so an event with it is a sample: handle it, but do not
// keep it for dedup.
const CheckInstallID = "00000000-0000-0000-0000-000000000000"

// CloudEvent is the envelope of every event the engine sends: a CloudEvents
// 1.0 event in its JSON form. A plugin that forwards events to a message
// broker or a webhook sends Raw, which is the event exactly as it arrived.
//
// Events arrive at least once. Two events with the same Source and ID are
// the same event: dedup by that pair.
type CloudEvent struct {
	SpecVersion string
	// ID is unique per Source and stable across redelivery.
	ID string
	// Source names the database's guard install: urn:uuid:<installId>.
	Source string
	// Type is one of the Type constants, or a type this SDK does not know.
	Type string
	// Time is when it happened; zero when the event has none (the
	// skipped-ahead marker).
	Time time.Time
	// Subject is the agent, agent:<name> with <name> its name in engine.json.
	Subject         string
	DataContentType string
	// Data is the event's data object as it arrived.
	Data json.RawMessage

	raw []byte
}

// Raw returns the event's exact JSON bytes, as the engine sent them. Forward
// these, not a JSON encoding of the Go value: FeedEvent and Notice are not
// CloudEvents in their JSON form.
func (e CloudEvent) Raw() []byte { return slices.Clone(e.raw) }

// IsCheckSample reports whether the event is one of the fixed samples
// basalt plugin check sends (its install id is CheckInstallID). Answer it as
// any other, but do not post it to a person or keep it for dedup.
func (e CloudEvent) IsCheckSample() bool { return e.Source == "urn:uuid:"+CheckInstallID }

// envelope is the JSON form of a CloudEvent.
type envelope struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Time            *time.Time      `json:"time"`
	Subject         string          `json:"subject"`
	DataContentType string          `json:"datacontenttype"`
	Data            json.RawMessage `json:"data"`
}

var errBadEvent = errors.New("basaltplugin: an event that is not a CloudEvent with a data object")

func decodeCloudEvent(raw json.RawMessage) (CloudEvent, error) {
	var e envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return CloudEvent{}, errBadEvent
	}
	if len(e.Data) == 0 || e.Data[0] != '{' {
		return CloudEvent{}, errBadEvent
	}
	out := CloudEvent{
		SpecVersion: e.SpecVersion, ID: e.ID, Source: e.Source, Type: e.Type, Subject: e.Subject,
		DataContentType: e.DataContentType, Data: slices.Clone(e.Data), raw: slices.Clone(raw),
	}
	if e.Time != nil {
		out.Time = *e.Time
	}
	return out, nil
}

// Op is the operation of a write.
type Op string

// The operations.
const (
	OpInsert Op = "INSERT"
	OpUpdate Op = "UPDATE"
	OpDelete Op = "DELETE"
	// OpOther is a refused statement that is none of the three.
	OpOther Op = "OTHER"
)

// FeedBatch is one feed.observe call: events of one agent, in ascending
// Seq, at most 100.
type FeedBatch struct {
	Agent  string
	Events []FeedEvent
}

// LastSeq is the Seq of the batch's last event: the answer of a plugin that
// handled all of it.
func (b FeedBatch) LastSeq() int64 {
	if len(b.Events) == 0 {
		return 0
	}
	return b.Events[len(b.Events)-1].Seq
}

// FeedEvent is one feed event. It holds metadata only: ids, the operation,
// the store's kind and verb, the resource, row counts, an error code, an expiry. Never a row value, a key,
// a statement, an intent or a link, so it is safe to store or forward.
type FeedEvent struct {
	CloudEvent
	// InstallID is the agent's database's guard install.
	InstallID string
	// Seq is the event's position in the database's ledger. Sequences are
	// shared by the database's agents, so one agent's events skip numbers.
	Seq int64
	// Agent is the agent, agent:<name> with <name> its name in engine.json.
	Agent string
	// Detail is the event's typed data: one of WriteApplied, WriteRefused,
	// WriteHeld, ApprovalSigned, RevertPrepared, RevertSigned,
	// RevertApplied, RevertRejected or FeedSkipped, chosen by Type. It is
	// nil for a Type this SDK does not know: skip such an event, and
	// still count it as handled.
	Detail FeedDetail
}

// FeedDetail is the typed data of a feed event. Switch on its type.
type FeedDetail interface{ feedDetail() }

// WriteApplied: a write was applied (TypeWriteApplied).
type WriteApplied struct {
	ChangeID string `json:"changeId"`
	// RunID and TaskID are empty when the write ran outside a run or task.
	RunID  string `json:"runId"`
	TaskID string `json:"taskId"`
	Op     Op     `json:"op"`
	// Store is the store's kind and its own word for the operation.
	Store Store `json:"store"`
	// Resource is the request's own resource in the store's form
	// (schema.table in Postgres), empty when it changed no row. It is the
	// store's text: show it as plain text, never as markup.
	Resource string `json:"resource"`
	// Rows is the rows written, cascades included.
	Rows         int64 `json:"rows"`
	CascadedRows int64 `json:"cascadedRows"`
}

// WriteRefused: a write was refused (TypeWriteRefused).
type WriteRefused struct {
	ChangeID string `json:"changeId"`
	RunID    string `json:"runId"`
	TaskID   string `json:"taskId"`
	Op       Op     `json:"op"`
	// Store is the store's kind and its own word for the operation; Verb
	// is empty on OpOther when the store did not give its command.
	Store Store `json:"store"`
	// ReasonCode is the refusal's error code, empty when it has none from
	// the contract's table.
	ReasonCode string `json:"reasonCode"`
}

// WriteHeld: a write was held for the person's approval (TypeWriteHeld).
// It names no resource.
type WriteHeld struct {
	ChangeID   string    `json:"changeId"`
	ApprovalID string    `json:"approvalId"`
	RunID      string    `json:"runId"`
	TaskID     string    `json:"taskId"`
	Op         Op        `json:"op"`
	Store      Store     `json:"store"`
	Rows       int64     `json:"rows"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// Store is an event's store part: the store's kind and its own word for
// the request's operation.
type Store struct {
	// Kind is the store's kind, such as StorePostgres.
	Kind StoreKind `json:"kind"`
	// Verb is the store's own word for the operation: in Postgres the
	// statement's command (INSERT, UPDATE, DELETE, or a DDL command).
	Verb string `json:"verb"`
}

// StoreKind is the kind of store an agent's action landed in.
type StoreKind string

// StorePostgres is the one store kind the engine serves.
const StorePostgres StoreKind = "POSTGRES"

// ApprovalSigned: the person signed a held write (TypeApprovalSigned).
type ApprovalSigned struct {
	ChangeID   string `json:"changeId"`
	ApprovalID string `json:"approvalId"`
}

// RevertPrepared: a revert was prepared for the person to sign
// (TypeRevertPrepared).
type RevertPrepared struct {
	RevertID string `json:"revertId"`
}

// RevertSigned: the person signed a large revert's plan (TypeRevertSigned).
type RevertSigned struct {
	ChangeID string `json:"changeId"`
	RevertID string `json:"revertId"`
}

// RevertApplied: a revert was applied (TypeRevertApplied).
type RevertApplied struct {
	ChangeID string `json:"changeId"`
	RevertID string `json:"revertId"`
}

// RevertRejected: the person rejected a revert (TypeRevertRejected).
type RevertRejected struct {
	RevertID string `json:"revertId"`
}

// FeedSkipped is the skipped-ahead marker (TypeFeedSkipped): the ledger's
// retention deleted events after AfterSeq before they were delivered. The
// agent's events with AfterSeq < seq <= the marker's Seq may be gone; how
// many, if any, is not known. Acknowledge it like any event.
type FeedSkipped struct {
	AfterSeq int64 `json:"afterSeq"`
}

func (WriteApplied) feedDetail()   {}
func (WriteRefused) feedDetail()   {}
func (WriteHeld) feedDetail()      {}
func (ApprovalSigned) feedDetail() {}
func (RevertPrepared) feedDetail() {}
func (RevertSigned) feedDetail()   {}
func (RevertApplied) feedDetail()  {}
func (RevertRejected) feedDetail() {}
func (FeedSkipped) feedDetail()    {}

// feedDetails maps each known feed type to a decoder of its data.
var feedDetails = map[string]func(json.RawMessage) (FeedDetail, error){
	TypeWriteApplied:   decodeDetail[WriteApplied],
	TypeWriteRefused:   decodeDetail[WriteRefused],
	TypeWriteHeld:      decodeDetail[WriteHeld],
	TypeApprovalSigned: decodeDetail[ApprovalSigned],
	TypeRevertPrepared: decodeDetail[RevertPrepared],
	TypeRevertSigned:   decodeDetail[RevertSigned],
	TypeRevertApplied:  decodeDetail[RevertApplied],
	TypeRevertRejected: decodeDetail[RevertRejected],
	TypeFeedSkipped:    decodeDetail[FeedSkipped],
}

func decodeDetail[T FeedDetail](data json.RawMessage) (FeedDetail, error) {
	var d T
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, errBadEvent
	}
	return d, nil
}

// decodeFeedEvent decodes one event of a batch. An unknown type is kept with
// no Detail; an unknown field anywhere is ignored.
func decodeFeedEvent(raw json.RawMessage) (FeedEvent, error) {
	ce, err := decodeCloudEvent(raw)
	if err != nil {
		return FeedEvent{}, err
	}
	var common struct {
		InstallID string `json:"installId"`
		Seq       *int64 `json:"seq"`
		Agent     string `json:"agent"`
	}
	if json.Unmarshal(ce.Data, &common) != nil || common.Seq == nil {
		return FeedEvent{}, errBadEvent
	}
	ev := FeedEvent{CloudEvent: ce, InstallID: common.InstallID, Seq: *common.Seq, Agent: common.Agent}
	if decode, ok := feedDetails[ce.Type]; ok {
		if ev.Detail, err = decode(ce.Data); err != nil {
			return FeedEvent{}, err
		}
	}
	return ev, nil
}

// NoticeKind is what a notice is about.
type NoticeKind string

// The notice kinds.
const (
	// KindApprovalRequested: the engine held a write (TypeApprovalRequested).
	KindApprovalRequested NoticeKind = "APPROVAL_REQUESTED"
	// KindRevertRequested: the engine prepared a revert (TypeRevertRequested).
	KindRevertRequested NoticeKind = "REVERT_REQUESTED"
	// KindLinkReplaced: the agent asked for a new link for a held write or
	// a revert; the earlier link no longer works (TypeLinkReplaced).
	KindLinkReplaced NoticeKind = "LINK_REPLACED"
)

// LinkOpens is where a notice's link opens.
type LinkOpens string

// Where a link opens.
const (
	// LinkOpensAnywhere: the portal; the link opens on any device.
	LinkOpensAnywhere LinkOpens = "ANYWHERE"
	// LinkOpensEngineComputer: the engine's local pages; the link opens
	// only on the computer Basalt runs on. Say so when you show it.
	LinkOpensEngineComputer LinkOpens = "ENGINE_COMPUTER"
)

// The six sentences a notice's Instruction can be: by item (a held write or
// a revert) and by where its link opens, or with no link.
const (
	InstructionWriteAnywhere        = "Open the link to review the held write and approve it with your passkey."
	InstructionWriteEngineComputer  = "Open the link on the computer Basalt runs on to review the held write and approve it with your passkey."
	InstructionWriteNoLink          = "Ask the agent for the link to review the held write and approve it with your passkey."
	InstructionRevertAnywhere       = "Open the link to review the revert and sign it with your passkey."
	InstructionRevertEngineComputer = "Open the link on the computer Basalt runs on to review the revert and sign it with your passkey."
	InstructionRevertNoLink         = "Ask the agent for the link to review the revert and sign it with your passkey."
)

// Notice is one approval.deliver call: something waits for the person. It is
// one-shot: never replayed from a cursor, and useless after ExpiresAt. A
// failed try is tried again, up to four times, with the same ID: dedup by
// Source and ID, as for feed events.
//
// Link is the link the agent's own answer handed over, byte for byte. A long
// link carries the whole request inside it (the statement, its parameters,
// the intent and the affected rows, values included), so whoever can read
// where you deliver it can read the request. A plugin that forwards notices
// to a message broker puts that link into the broker's log, for as long as
// the broker keeps it. Every other field holds no value, statement or
// intent.
type Notice struct {
	CloudEvent
	Kind NoticeKind
	// Agent is the agent, agent:<name> with <name> its name in engine.json.
	Agent string
	// Exactly one of ApprovalID (a held write) and RevertID (a revert) is set.
	ApprovalID string
	RevertID   string
	// Headline is the item in one line, plain text: show it as text, never
	// parse it or render it as markup.
	Headline  string
	ExpiresAt time.Time
	// Link is empty when the agent's answer had no link that stands alone;
	// Instruction then says to ask the agent.
	Link      string
	LinkOpens LinkOpens
	// Instruction is what the person does: one of the Instruction
	// constants.
	Instruction string
}

// noticeData is the JSON form of a notice's data. It names only the
// contract's fields, so a later field, whatever its name, is ignored.
type noticeData struct {
	Kind        NoticeKind `json:"kind"`
	Agent       string     `json:"agent"`
	ApprovalID  string     `json:"approvalId"`
	RevertID    string     `json:"revertId"`
	Headline    string     `json:"headline"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	Link        string     `json:"link"`
	LinkOpens   LinkOpens  `json:"linkOpens"`
	Instruction string     `json:"instruction"`
}

// decodeNotice decodes a notice. An unknown type or field is not an error:
// the notice reaches the plugin, which skips what it does not know.
func decodeNotice(raw json.RawMessage) (Notice, error) {
	ce, err := decodeCloudEvent(raw)
	if err != nil {
		return Notice{}, err
	}
	var d noticeData
	if json.Unmarshal(ce.Data, &d) != nil {
		return Notice{}, errBadEvent
	}
	return Notice{
		CloudEvent: ce, Kind: d.Kind, Agent: d.Agent, ApprovalID: d.ApprovalID, RevertID: d.RevertID,
		Headline: d.Headline, ExpiresAt: d.ExpiresAt, Link: d.Link, LinkOpens: d.LinkOpens, Instruction: d.Instruction,
	}, nil
}
