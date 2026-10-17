package billdogeng

// Tier-3: CaptureException → canonical `$exception` event (PostHog error-tracking
// parity), mirroring node/test/capture-exception.test.ts.

import (
	"net/http"
	"testing"
)

// checkoutError is a concrete error type so `%T` yields an assertable class name.
type checkoutError struct{ msg string }

func (e *checkoutError) Error() string { return e.msg }

// exceptionStub replies with a 200 envelope on /ingest-events and 404 elsewhere.
func exceptionStub() *stubServer {
	return startStub(func(req capturedRequest, w http.ResponseWriter, _ int) {
		if req.Path == "/ingest-events" {
			sendEnvelope(w, map[string]interface{}{}, 200)
			return
		}
		sendRaw(w, map[string]interface{}{}, 404)
	})
}

func TestCaptureExceptionCanonicalShape(t *testing.T) {
	s := exceptionStub()
	defer s.close()

	bd, err := New("bd_test_x", Options{Host: s.URL, FlushAt: 1000})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	bd.CaptureException(&checkoutError{msg: "boom"}, "alice", Properties{"route": "/checkout"})
	if err := bd.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	evts := ingested(s)
	if len(evts) != 1 {
		t.Fatalf("want 1 event, got %d", len(evts))
	}
	e := evts[0]
	if e["event_name"] != "$exception" {
		t.Errorf("event_name = %v, want $exception", e["event_name"])
	}
	if e["user_id"] != "alice" {
		t.Errorf("user_id = %v, want alice", e["user_id"])
	}
	props, _ := e["properties"].(map[string]interface{})
	if props["route"] != "/checkout" {
		t.Errorf("merged prop route = %v, want /checkout", props["route"])
	}
	if props["$exception_type"] != "*billdogeng.checkoutError" {
		t.Errorf("$exception_type = %v, want *billdogeng.checkoutError", props["$exception_type"])
	}
	if props["$exception_message"] != "boom" {
		t.Errorf("$exception_message = %v, want boom", props["$exception_message"])
	}
	if props["$exception_level"] != "error" {
		t.Errorf("$exception_level = %v, want error", props["$exception_level"])
	}

	list, ok := props["$exception_list"].([]interface{})
	if !ok || len(list) != 1 {
		t.Fatalf("$exception_list = %v, want 1-element array", props["$exception_list"])
	}
	entry, _ := list[0].(map[string]interface{})
	if entry["type"] != "*billdogeng.checkoutError" {
		t.Errorf("entry.type = %v", entry["type"])
	}
	if entry["value"] != "boom" {
		t.Errorf("entry.value = %v", entry["value"])
	}
	mech, _ := entry["mechanism"].(map[string]interface{})
	if mech["handled"] != true || mech["synthetic"] != false {
		t.Errorf("mechanism = %v, want {handled:true, synthetic:false}", mech)
	}
	st, _ := entry["stacktrace"].(map[string]interface{})
	if st["type"] != "raw" {
		t.Errorf("stacktrace.type = %v, want raw", st["type"])
	}
	frames, _ := st["frames"].([]interface{})
	if len(frames) == 0 {
		t.Fatalf("want >=1 frame, got 0")
	}
	// The last frame (oldest-first ordering) carries the expected fields/types.
	last, _ := frames[len(frames)-1].(map[string]interface{})
	if _, ok := last["filename"].(string); !ok {
		t.Errorf("frame.filename not a string: %v", last["filename"])
	}
	// JSON numbers decode to float64.
	if _, ok := last["lineno"].(float64); !ok {
		t.Errorf("frame.lineno not a number: %v", last["lineno"])
	}
	if _, ok := last["in_app"].(bool); !ok {
		t.Errorf("frame.in_app not a bool: %v", last["in_app"])
	}

	_ = bd.Shutdown()
}

func TestCaptureExceptionStacklessValue(t *testing.T) {
	s := exceptionStub()
	defer s.close()

	bd, err := New("bd_test_x", Options{Host: s.URL, FlushAt: 1000})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A nil error is Go's analog of a non-exception / stackless value: default type,
	// its string form as the message, and no frames.
	bd.CaptureException(nil, "bob", nil)
	if err := bd.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	evts := ingested(s)
	if len(evts) != 1 {
		t.Fatalf("want 1 event, got %d", len(evts))
	}
	props, _ := evts[0]["properties"].(map[string]interface{})
	if props["$exception_type"] != "error" {
		t.Errorf("$exception_type = %v, want error", props["$exception_type"])
	}
	if props["$exception_message"] != "<nil>" {
		t.Errorf("$exception_message = %v, want <nil>", props["$exception_message"])
	}
	list, _ := props["$exception_list"].([]interface{})
	entry, _ := list[0].(map[string]interface{})
	st, _ := entry["stacktrace"].(map[string]interface{})
	frames, _ := st["frames"].([]interface{})
	if len(frames) != 0 {
		t.Errorf("frames = %v, want empty", frames)
	}

	_ = bd.Shutdown()
}

func TestCaptureExceptionInertWhenDisabled(t *testing.T) {
	s := startStub(func(_ capturedRequest, w http.ResponseWriter, _ int) {
		sendRaw(w, map[string]interface{}{}, 500)
	})
	defer s.close()

	bd, err := New("bd_test_x", Options{Host: s.URL, Disabled: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	bd.CaptureException(&checkoutError{msg: "x"}, "alice", nil)
	if n := bd.QueueLength(); n != 0 {
		t.Errorf("QueueLength = %d, want 0 (disabled)", n)
	}
	_ = bd.Shutdown()
}
