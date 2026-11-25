package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const testdata = "../testdata"

func load(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testdata, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func TestContractExamplesDecode(t *testing.T) {
	var req InitializeRequest
	load(t, "valid-initialize-request.json", &req)
	if len(req.Protocols) == 0 || len(req.Hooks) == 0 || req.Limits.MaxInFlight != MaxInFlight || req.Limits.MaxMessageBytes != MaxMessageBytes {
		t.Fatalf("request = %+v", req)
	}
	var resp InitializeResponse
	load(t, "valid-initialize-response.json", &resp)
	if resp.Protocol != Version1 || resp.Name == "" || len(resp.Hooks) == 0 {
		t.Fatalf("response = %+v", resp)
	}
	var h HealthResponse
	load(t, "valid-health-response.json", &h)
	if h.Status != HealthReady {
		t.Fatalf("health = %+v", h)
	}
}

func TestNotifierExamplesDecode(t *testing.T) {
	var feed FeedObserveRequest
	load(t, "valid-feed-request.json", &feed)
	var events []json.RawMessage
	if err := json.Unmarshal(feed.Events, &events); err != nil || feed.Agent != "agent:product_a" || len(events) != 7 {
		t.Fatalf("feed request = %+v (%v)", feed, err)
	}
	var res FeedObserveResponse
	load(t, "valid-feed-response.json", &res)
	if res.LastSeq != 4231 {
		t.Fatalf("feed response = %+v", res)
	}
	var notice ApprovalDeliverRequest
	load(t, "valid-deliver-request-check-sample.json", &notice)
	if notice.Agent != "agent:product_a" || !json.Valid(notice.Event) || notice.Event[0] != '{' {
		t.Fatalf("deliver request = %+v", notice)
	}
	var empty ApprovalDeliverResponse
	load(t, "valid-deliver-response.json", &empty)
}

// TestMarshalKeepsEventBytes: a transport sends the event bytes as the engine
// built them, compacted but with no character escaped.
func TestMarshalKeepsEventBytes(t *testing.T) {
	b, err := Marshal(FeedObserveRequest{Agent: "a", Events: json.RawMessage(`[ {"resource": "a<b&c>d"} ]`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"agent":"a","events":[{"resource":"a<b&c>d"}]}` {
		t.Fatalf("got %s", b)
	}
}

func TestEmptyConfigEncodesAsObject(t *testing.T) {
	b, err := json.Marshal(InitializeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["config"]) != "{}" {
		t.Fatalf("config = %s", m["config"])
	}
}
