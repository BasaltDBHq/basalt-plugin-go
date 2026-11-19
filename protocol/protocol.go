// Package protocol holds the Go types for the Basalt plugin protocol messages
// that every transport carries: initialize, health and shutdown, the notifier
// hooks, and the hooks declaration shared with the manifest. The field names
// match the JSON schemas of the engine's contract; unknown JSON fields are
// ignored on decode.
//
// Plugin authors use package basaltplugin, which speaks the protocol for
// them. This package is for code that speaks it directly, as the engine does.
package protocol

// Version1 is the only protocol version there is.
const Version1 = "basalt-plugin/1"

// The lifecycle methods, as the JSON-RPC method names. ConnectRPC maps them
// to PluginService/Initialize, Health and Shutdown.
const (
	MethodInitialize = "initialize"
	MethodHealth     = "health"
	MethodShutdown   = "shutdown"
)

// Limits every connection runs under.
const (
	// MaxMessageBytes is the largest message either side may send.
	MaxMessageBytes = 4 << 20
	// MaxInFlight is the number of hook calls the engine keeps open on one
	// entry. health and shutdown are not counted: each has a slot of its own.
	MaxInFlight = 4
)

// HookDecl is what a plugin declares for one hook, in the manifest and in the
// initialize reply.
type HookDecl struct {
	Contract    string   `json:"contract,omitempty"`
	Roles       []string `json:"roles,omitempty"`
	EntityTypes []string `json:"entityTypes,omitempty"`
}

// Limits is sent to the plugin in initialize.
type Limits struct {
	MaxMessageBytes int `json:"maxMessageBytes"`
	MaxInFlight     int `json:"maxInFlight"`
	TimeoutMs       int `json:"timeoutMs"`
}

// InitializeRequest is the first call on every connection.
type InitializeRequest struct {
	Protocols []string `json:"protocols"`
	Hooks     []string `json:"hooks"`
	// Config is the entry's config from engine.json, {} when it has none.
	Config RawObject `json:"config"`
	Limits Limits    `json:"limits"`
}

// Model names the model behind a plugin, when it has one.
type Model struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResponse is the plugin's reply.
type InitializeResponse struct {
	Protocol string              `json:"protocol"`
	Name     string              `json:"name"`
	Version  string              `json:"version"`
	Hooks    map[string]HookDecl `json:"hooks"`
	Model    *Model              `json:"model,omitempty"`
}

// HealthRequest is empty.
type HealthRequest struct{}

// Health statuses.
const (
	HealthReady    = "READY"
	HealthNotReady = "NOT_READY"
)

// HealthResponse is the plugin's reply to health.
type HealthResponse struct {
	Status string `json:"status"`
}

// ShutdownRequest is empty.
type ShutdownRequest struct{}

// ShutdownResponse is empty.
type ShutdownResponse struct{}

// RawObject is a JSON object kept as bytes. A nil value encodes as {}.
type RawObject []byte

// MarshalJSON returns the object, or {} when there is none.
func (r RawObject) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("{}"), nil
	}
	return r, nil
}

// UnmarshalJSON keeps a copy of the object's bytes.
func (r *RawObject) UnmarshalJSON(b []byte) error {
	*r = append((*r)[:0], b...)
	return nil
}
