package billdogeng

import (
	"net/http"
	"testing"
	"time"
)

// personPropsStub answers /user-properties and /ingest-events with envelopes.
func personPropsStub() *stubServer {
	return startStub(func(req capturedRequest, w http.ResponseWriter, _ int) {
		switch req.Path {
		case "/user-properties":
			sendEnvelope(w, map[string]interface{}{"updated": true}, 200)
		case "/ingest-events":
			sendEnvelope(w, map[string]interface{}{"processed": 1}, 200)
		default:
			sendRaw(w, map[string]interface{}{}, 404)
		}
	})
}

// waitForUserProps polls for a /user-properties request (the call is fire-and-forget).
func waitForUserProps(s *stubServer, timeout time.Duration) (capturedRequest, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r, ok := s.findPath("/user-properties"); ok {
			return r, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return capturedRequest{}, false
}

func opAt(t *testing.T, ops []interface{}, i int) map[string]interface{} {
	t.Helper()
	if i >= len(ops) {
		t.Fatalf("operation index %d out of range (len %d)", i, len(ops))
	}
	m, ok := ops[i].(map[string]interface{})
	if !ok {
		t.Fatalf("operation %d is not an object: %v", i, ops[i])
	}
	return m
}

// #3: SetPersonProperties maps set/setOnce/unset to the operations wire shape,
// in order, and POSTs to /user-properties (no project_id, X-BillDog-API-Key header).
func TestSetPersonPropertiesWireShape(t *testing.T) {
	s := personPropsStub()
	defer s.close()

	bd, _ := New("bd_test_x", Options{Host: s.URL})
	bd.SetPersonProperties("alice", PersonPropertyOps{
		Set:     map[string]interface{}{"plan": "pro"},
		SetOnce: map[string]interface{}{"signup": "2026-01-01"},
		Unset:   []string{"tmp"},
	})

	req, ok := waitForUserProps(s, time.Second)
	if !ok {
		t.Fatal("no /user-properties request")
	}
	if req.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.Method)
	}
	if req.Headers.Get("X-BillDog-API-Key") != "bd_test_x" {
		t.Errorf("X-BillDog-API-Key = %q", req.Headers.Get("X-BillDog-API-Key"))
	}
	if _, sent := req.Headers[http.CanonicalHeaderKey("x-api-key")]; sent {
		t.Errorf("legacy x-api-key header must not be sent")
	}
	if _, present := req.Body["project_id"]; present {
		t.Errorf("body must NOT contain project_id: %v", req.Body)
	}
	if req.Body["api_key"] != "bd_test_x" {
		t.Errorf("api_key = %v", req.Body["api_key"])
	}
	if req.Body["distinct_id"] != "alice" {
		t.Errorf("distinct_id = %v", req.Body["distinct_id"])
	}
	ops, _ := req.Body["operations"].([]interface{})
	if len(ops) != 3 {
		t.Fatalf("operations = %d, want 3: %v", len(ops), req.Body["operations"])
	}
	set := opAt(t, ops, 0)
	if set["operation"] != "$set" || set["key"] != "plan" || set["value"] != "pro" {
		t.Errorf("op[0] = %v", set)
	}
	once := opAt(t, ops, 1)
	if once["operation"] != "$set_once" || once["key"] != "signup" || once["value"] != "2026-01-01" {
		t.Errorf("op[1] = %v", once)
	}
	unset := opAt(t, ops, 2)
	if unset["operation"] != "$unset" || unset["key"] != "tmp" {
		t.Errorf("op[2] = %v", unset)
	}
	if _, present := unset["value"]; present {
		t.Errorf("$unset op must not carry a value: %v", unset)
	}

	_ = bd.Shutdown()
}

// #3: empty ops issue NO /user-properties request.
func TestSetPersonPropertiesNoOpWhenEmpty(t *testing.T) {
	s := personPropsStub()
	defer s.close()

	bd, _ := New("bd_test_x", Options{Host: s.URL})
	bd.SetPersonProperties("alice", PersonPropertyOps{})

	if _, ok := waitForUserProps(s, 150*time.Millisecond); ok {
		t.Fatal("expected NO /user-properties request for empty ops")
	}
	_ = bd.Shutdown()
}

// #3: IdentifyWithOptions persists $set_once via /user-properties AND still emits
// the $identify event to /ingest-events.
func TestIdentifyWithOptionsSetOnce(t *testing.T) {
	s := personPropsStub()
	defer s.close()

	bd, _ := New("bd_test_x", Options{Host: s.URL, FlushAt: 1000})
	bd.IdentifyWithOptions("alice", Properties{"email": "a@b.co"}, IdentifyOptions{
		SetOnce: map[string]interface{}{"first_seen": "2026-01-01"},
	})

	req, ok := waitForUserProps(s, time.Second)
	if !ok {
		t.Fatal("no /user-properties request")
	}
	ops, _ := req.Body["operations"].([]interface{})
	if len(ops) != 1 {
		t.Fatalf("operations = %d, want 1: %v", len(ops), req.Body["operations"])
	}
	once := opAt(t, ops, 0)
	if once["operation"] != "$set_once" || once["key"] != "first_seen" || once["value"] != "2026-01-01" {
		t.Errorf("op[0] = %v", once)
	}

	if err := bd.Flush(); err != nil {
		t.Fatal(err)
	}
	identifyCount := 0
	for _, r := range s.requestsForPath("/ingest-events") {
		events, _ := r.Body["events"].([]interface{})
		for _, e := range events {
			if m, ok := e.(map[string]interface{}); ok && m["event_name"] == "$identify" {
				identifyCount++
			}
		}
	}
	if identifyCount != 1 {
		t.Fatalf("$identify events = %d, want 1", identifyCount)
	}
	_ = bd.Shutdown()
}

// #3: default Identify (no setOnce) does NOT call /user-properties.
func TestIdentifyDefaultNoUserProperties(t *testing.T) {
	s := personPropsStub()
	defer s.close()

	bd, _ := New("bd_test_x", Options{Host: s.URL, FlushAt: 1000})
	bd.Identify("alice", Properties{"email": "a@b.co"})

	if _, ok := waitForUserProps(s, 150*time.Millisecond); ok {
		t.Fatal("default Identify must NOT hit /user-properties")
	}
	_ = bd.Shutdown()
}
