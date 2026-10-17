package billdogeng

import (
	"net/http"
	"strings"
	"testing"
)

// Spec test #1: 10 captures -> ONE batched POST with 10 events.
func TestBatching10CapturesOnePost(t *testing.T) {
	stub := startStub(func(_ capturedRequest, w http.ResponseWriter, _ int) {
		sendEnvelope(w, map[string]interface{}{"processed": 10}, 200)
	})
	defer stub.close()

	bd, err := New("bd_test_x", Options{Host: stub.URL, FlushAt: 100})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		bd.Capture("user-123", "order_completed", Properties{"n": i}, nil)
	}
	if got := bd.QueueLength(); got != 10 {
		t.Fatalf("queue length = %d, want 10", got)
	}

	if err := bd.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := bd.Shutdown(); err != nil {
		t.Fatal(err)
	}

	ingest := stub.requestsForPath("/ingest-events")
	if len(ingest) != 1 {
		t.Fatalf("ingest requests = %d, want 1", len(ingest))
	}
	body := ingest[0].Body
	if body["api_key"] != "bd_test_x" {
		t.Errorf("api_key = %v, want bd_test_x", body["api_key"])
	}
	events, _ := body["events"].([]interface{})
	if len(events) != 10 {
		t.Fatalf("events = %d, want 10", len(events))
	}
	ev := events[0].(map[string]interface{})
	if ev["event_name"] != "order_completed" {
		t.Errorf("event_name = %v", ev["event_name"])
	}
	if ev["user_id"] != "user-123" {
		t.Errorf("user_id = %v", ev["user_id"])
	}
	if _, ok := ev["event_timestamp"].(float64); !ok {
		t.Errorf("event_timestamp not a number: %v", ev["event_timestamp"])
	}
	props := ev["properties"].(map[string]interface{})
	if props["n"].(float64) != 0 {
		t.Errorf("properties.n = %v, want 0", props["n"])
	}
	if ingest[0].Headers.Get("X-BillDog-API-Key") != "bd_test_x" {
		t.Errorf("X-BillDog-API-Key header = %q", ingest[0].Headers.Get("X-BillDog-API-Key"))
	}
	if _, sent := ingest[0].Headers[http.CanonicalHeaderKey("x-api-key")]; sent {
		t.Errorf("legacy x-api-key header must not be sent")
	}
}

func TestAutoFlushAtFlushAt(t *testing.T) {
	stub := startStub(func(_ capturedRequest, w http.ResponseWriter, _ int) {
		sendEnvelope(w, map[string]interface{}{"processed": 5}, 200)
	})
	defer stub.close()

	bd, _ := New("bd_test_x", Options{Host: stub.URL, FlushAt: 5})
	for i := 0; i < 5; i++ {
		bd.Capture("u", "e", Properties{"i": i}, nil)
	}
	_ = bd.Flush()
	_ = bd.Shutdown()

	ingest := stub.requestsForPath("/ingest-events")
	if len(ingest) < 1 {
		t.Fatalf("ingest requests = %d, want >=1", len(ingest))
	}
	total := 0
	for _, r := range ingest {
		events, _ := r.Body["events"].([]interface{})
		total += len(events)
	}
	if total != 5 {
		t.Fatalf("total events = %d, want 5", total)
	}
}

// Spec test #2: identify / groupIdentify / alias event shapes.
func TestIdentifyShape(t *testing.T) {
	stub := startStub(func(_ capturedRequest, w http.ResponseWriter, _ int) {
		sendEnvelope(w, map[string]interface{}{"processed": 1}, 200)
	})
	defer stub.close()

	bd, _ := New("bd_test_x", Options{Host: stub.URL, FlushAt: 100})
	bd.Identify("user-123", Properties{"email": "a@b.com", "plan": "pro"})
	_ = bd.Flush()
	_ = bd.Shutdown()

	ev := firstEvent(t, stub)
	if ev["event_name"] != "$identify" {
		t.Errorf("event_name = %v", ev["event_name"])
	}
	if ev["user_id"] != "user-123" {
		t.Errorf("user_id = %v", ev["user_id"])
	}
	props := ev["properties"].(map[string]interface{})
	set := props["$set"].(map[string]interface{})
	if set["email"] != "a@b.com" || set["plan"] != "pro" {
		t.Errorf("$set = %v", set)
	}
}

func TestGroupIdentifyShape(t *testing.T) {
	stub := startStub(func(_ capturedRequest, w http.ResponseWriter, _ int) {
		sendEnvelope(w, map[string]interface{}{"processed": 1}, 200)
	})
	defer stub.close()

	bd, _ := New("bd_test_x", Options{Host: stub.URL, FlushAt: 100})
	bd.GroupIdentify("company", "acme", Properties{"seats": 50})
	_ = bd.Flush()
	_ = bd.Shutdown()

	ev := firstEvent(t, stub)
	if ev["event_name"] != "$groupidentify" {
		t.Errorf("event_name = %v", ev["event_name"])
	}
	if ev["user_id"] != "acme" {
		t.Errorf("user_id = %v", ev["user_id"])
	}
	props := ev["properties"].(map[string]interface{})
	if props["$group_type"] != "company" {
		t.Errorf("$group_type = %v", props["$group_type"])
	}
	if props["$group_key"] != "acme" {
		t.Errorf("$group_key = %v", props["$group_key"])
	}
	set := props["$group_set"].(map[string]interface{})
	if set["seats"].(float64) != 50 {
		t.Errorf("$group_set.seats = %v", set["seats"])
	}
}

func TestAliasShape(t *testing.T) {
	stub := startStub(func(_ capturedRequest, w http.ResponseWriter, _ int) {
		sendEnvelope(w, map[string]interface{}{"processed": 1}, 200)
	})
	defer stub.close()

	bd, _ := New("bd_test_x", Options{Host: stub.URL, FlushAt: 100})
	bd.Alias("user-123", "anon-abc")
	_ = bd.Flush()
	_ = bd.Shutdown()

	ev := firstEvent(t, stub)
	if ev["event_name"] != "$create_alias" {
		t.Errorf("event_name = %v", ev["event_name"])
	}
	props := ev["properties"].(map[string]interface{})
	if props["alias"] != "anon-abc" {
		t.Errorf("alias = %v", props["alias"])
	}
}

func TestCaptureSendsGroupsOnly(t *testing.T) {
	stub := startStub(func(_ capturedRequest, w http.ResponseWriter, _ int) {
		sendEnvelope(w, map[string]interface{}{"processed": 1}, 200)
	})
	defer stub.close()

	bd, _ := New("bd_test_x", Options{
		Host:    stub.URL,
		FlushAt: 100,
	})
	bd.Capture("user-123", "event", Properties{"x": 1}, Groups{"company": "acme", "team": "core"})
	_ = bd.Flush()
	_ = bd.Shutdown()

	ev := firstEvent(t, stub)
	props := ev["properties"].(map[string]interface{})
	groups := props["$groups"].(map[string]interface{})
	if groups["company"] != "acme" || groups["team"] != "core" {
		t.Errorf("$groups = %v", groups)
	}
	// `$groups` is the only group wire shape: no positional $group_N keys.
	for k := range props {
		if strings.HasPrefix(k, "$group_") {
			t.Errorf("unexpected positional group key %q", k)
		}
	}
}

func firstEvent(t *testing.T, stub *stubServer) map[string]interface{} {
	t.Helper()
	ingest := stub.requestsForPath("/ingest-events")
	if len(ingest) == 0 {
		t.Fatal("no ingest requests")
	}
	events, _ := ingest[0].Body["events"].([]interface{})
	if len(events) == 0 {
		t.Fatal("no events in first ingest request")
	}
	return events[0].(map[string]interface{})
}
