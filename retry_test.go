package billdogeng

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

// Spec test #4: 503 then 200 -> succeeds after backoff, single batch delivered.
func TestRetry503Then200(t *testing.T) {
	stub := startStub(func(_ capturedRequest, w http.ResponseWriter, hits int) {
		if hits == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   map[string]interface{}{"code": "UNAVAILABLE", "message": "try later"},
			})
			return
		}
		sendEnvelope(w, map[string]interface{}{"processed": 1}, 200)
	})
	defer stub.close()

	bd, _ := New("bd_test_x", Options{Host: stub.URL, FlushAt: 100, MaxRetries: 3})
	bd.Capture("user-123", "event", Properties{"a": 1}, nil)

	started := time.Now()
	if err := bd.Flush(); err != nil {
		t.Fatalf("flush should not error: %v", err)
	}
	elapsed := time.Since(started)
	_ = bd.Shutdown()

	ingest := stub.requestsForPath("/ingest-events")
	if len(ingest) != 2 {
		t.Fatalf("ingest attempts = %d, want 2", len(ingest))
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %s, want >= 900ms (backoff)", elapsed)
	}
	if bd.QueueLength() != 0 {
		t.Errorf("queue length = %d, want 0 (drained)", bd.QueueLength())
	}
}

func TestRetryExhaustionRequeues(t *testing.T) {
	stub := startStub(func(_ capturedRequest, w http.ResponseWriter, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   map[string]interface{}{"code": "BOOM", "message": "always down"},
		})
	})
	defer stub.close()

	bd, _ := New("bd_test_x", Options{Host: stub.URL, FlushAt: 100, MaxRetries: 1})
	bd.Capture("user-123", "event", nil, nil)

	err := bd.Flush()
	var be *Error
	if !errors.As(err, &be) {
		t.Fatalf("flush error = %v, want *Error", err)
	}
	if bd.QueueLength() != 1 {
		t.Errorf("queue length = %d, want 1 (re-queued)", bd.QueueLength())
	}
	_ = bd.Shutdown()

	ingest := stub.requestsForPath("/ingest-events")
	if len(ingest) < 2 {
		t.Errorf("ingest attempts = %d, want >= 2", len(ingest))
	}
}

func TestNoRetryOn4xx(t *testing.T) {
	stub := startStub(func(_ capturedRequest, w http.ResponseWriter, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   map[string]interface{}{"code": "BAD", "message": "bad request"},
		})
	})
	defer stub.close()

	bd, _ := New("bd_test_x", Options{Host: stub.URL, FlushAt: 100, MaxRetries: 3})
	bd.Capture("u", "e", nil, nil)
	err := bd.Flush()
	var be *Error
	if !errors.As(err, &be) || be.Status != 400 {
		t.Fatalf("error = %v, want status 400", err)
	}
	ingest := stub.requestsForPath("/ingest-events")
	if len(ingest) != 1 {
		t.Errorf("ingest attempts = %d, want 1 (no retry on 4xx)", len(ingest))
	}
	_ = bd.Shutdown()
}
