package billdogeng

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The server's own prose for an exhausted meter — distinctive enough to count
// occurrences of it in the captured log.
const quotaLimitedMessage = "Your free plan's monthly events allowance (1,000,000) is exhausted — this data was not stored. Upgrade to keep recording."

// sendQuotaLimited answers with the canonical over-quota 200: success, a
// top-level `quota_limited` array, the diagnostic `quota` block and the prose.
// See supabase/functions/_shared/quota-limited.ts.
func sendQuotaLimited(w http.ResponseWriter) {
	sendRaw(w, map[string]interface{}{
		"success":       true,
		"quota_limited": []string{"events"},
		"quota":         map[string]interface{}{"scope": "plan", "metric": "events", "cap": 1000000},
		"message":       quotaLimitedMessage,
	}, 200)
}

// captureLog redirects the standard logger for the duration of fn and returns
// what was written — warnf is deliberately ungated, so this is how a test proves
// the "exactly once" part of the contract.
func captureLog(fn func()) string {
	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()
	fn()
	return buf.String()
}

// (a) A 2xx carrying `quota_limited` is NOT a success: the flush reports the
// refusal, the batch is dropped rather than re-queued (it can never drain), the
// reason is logged once, and further captures are no-ops so memory cannot grow.
func TestQuotaLimited200SuspendsAndDoesNotReportSuccess(t *testing.T) {
	stub := startStub(func(_ capturedRequest, w http.ResponseWriter, _ int) {
		sendQuotaLimited(w)
	})
	defer stub.close()

	bd, err := New("bd_test_x", Options{Host: stub.URL, FlushAt: 100, FlushIntervalMs: 60000, MaxRetries: 3})
	if err != nil {
		t.Fatal(err)
	}

	logged := captureLog(func() {
		bd.Capture("user-123", "order_completed", Properties{"n": 1}, nil)
		if err := bd.Flush(); err == nil {
			t.Fatal("flush reported success for a batch the server discarded")
		} else if !errors.Is(err, ErrQuotaLimited) {
			t.Fatalf("flush error = %v, want ErrQuotaLimited", err)
		}

		// Suspended: capture is a no-op and flush sends nothing more.
		bd.Capture("user-123", "order_completed", Properties{"n": 2}, nil)
		bd.Identify("user-123", Properties{"plan": "free"})
		if got := bd.QueueLength(); got != 0 {
			t.Errorf("queue length = %d, want 0 (nothing buffered while suspended)", got)
		}
		_ = bd.Flush()
	})

	// One attempt only — a refused payload is terminal, not retryable.
	if ingest := stub.requestsForPath("/ingest-events"); len(ingest) != 1 {
		t.Fatalf("ingest requests = %d, want 1 (no retries, no flushing while suspended)", len(ingest))
	}
	if n := strings.Count(logged, quotaLimitedMessage); n != 1 {
		t.Errorf("reason logged %d time(s), want exactly 1:\n%s", n, logged)
	}
	if !strings.Contains(logged, "WARN") {
		t.Errorf("reason not logged at warn level:\n%s", logged)
	}

	// Shutdown must neither hang nor panic while suspended.
	done := make(chan struct{})
	go func() {
		_ = bd.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown hung while suspended")
	}
}

// (b) A normal 2xx still succeeds — including one that carries an EMPTY
// `quota_limited`, which means "stored fine".
func TestQuotaLimitedAbsentOrEmptyIsSuccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(w http.ResponseWriter)
	}{
		{"envelope", func(w http.ResponseWriter) { sendEnvelope(w, map[string]interface{}{"processed": 1}, 200) }},
		{"empty array", func(w http.ResponseWriter) {
			sendRaw(w, map[string]interface{}{"success": true, "quota_limited": []string{}}, 200)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := startStub(func(_ capturedRequest, w http.ResponseWriter, _ int) { tc.send(w) })
			defer stub.close()

			bd, _ := New("bd_test_x", Options{Host: stub.URL, FlushAt: 100, FlushIntervalMs: 60000})
			bd.Capture("user-123", "event", Properties{"a": 1}, nil)
			if err := bd.Flush(); err != nil {
				t.Fatalf("flush error = %v, want nil", err)
			}
			if bd.QueueLength() != 0 {
				t.Errorf("queue length = %d, want 0 (drained)", bd.QueueLength())
			}
			// Not suspended: the next capture is still queued.
			bd.Capture("user-123", "event", Properties{"a": 2}, nil)
			if bd.QueueLength() != 1 {
				t.Errorf("queue length = %d, want 1 (capture still live)", bd.QueueLength())
			}
			_ = bd.Shutdown()
		})
	}
}

// (c) A quota_limited key that carries no live signal reads as "not limited" —
// the flush succeeds and nothing escapes the transport path.
func TestQuotaLimitedMalformedBodyIsSuccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(w http.ResponseWriter)
	}{
		{"wrong type", func(w http.ResponseWriter) {
			sendRaw(w, map[string]interface{}{"quota_limited": "events"}, 200)
		}},
		{"null entries", func(w http.ResponseWriter) {
			sendRaw(w, map[string]interface{}{"quota_limited": []interface{}{nil, ""}}, 200)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := startStub(func(_ capturedRequest, w http.ResponseWriter, _ int) { tc.send(w) })
			defer stub.close()

			bd, _ := New("bd_test_x", Options{Host: stub.URL, FlushAt: 100, FlushIntervalMs: 60000})
			bd.Capture("user-123", "event", Properties{"a": 1}, nil)
			if err := bd.Flush(); err != nil {
				t.Fatalf("flush error = %v, want nil (a body we cannot read is not a refusal)", err)
			}
			bd.Capture("user-123", "event", Properties{"a": 2}, nil)
			if bd.QueueLength() != 1 {
				t.Errorf("queue length = %d, want 1 (not suspended)", bd.QueueLength())
			}
			_ = bd.Shutdown()
		})
	}
}

// (d) A 2xx that is not the {success, data} envelope is not a BillDog answer,
// so it is not proof of delivery: the flush fails MALFORMED_RESPONSE and the
// batch is kept for the next one — never suspended, never read as a payload.
func TestNonEnvelopeBodyIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(w http.ResponseWriter)
	}{
		{"not json", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("ok"))
		}},
		{"empty body", func(w http.ResponseWriter) { w.WriteHeader(200) }},
		{"json array", func(w http.ResponseWriter) { sendRaw(w, []interface{}{"quota_limited"}, 200) }},
		{"bare object", func(w http.ResponseWriter) {
			sendRaw(w, map[string]interface{}{"processed": 1}, 200)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := startStub(func(_ capturedRequest, w http.ResponseWriter, _ int) { tc.send(w) })
			defer stub.close()

			bd, _ := New("bd_test_x", Options{Host: stub.URL, FlushAt: 100, FlushIntervalMs: 60000})
			bd.Capture("user-123", "event", Properties{"a": 1}, nil)
			err := bd.Flush()
			var e *Error
			if !errors.As(err, &e) || e.Code != "MALFORMED_RESPONSE" {
				t.Fatalf("flush error = %v, want MALFORMED_RESPONSE", err)
			}
			if errors.Is(err, ErrQuotaLimited) {
				t.Fatalf("a malformed body must not read as a quota refusal")
			}
			if bd.QueueLength() != 1 {
				t.Errorf("queue length = %d, want 1 (batch kept for the next flush)", bd.QueueLength())
			}
			_ = bd.Shutdown()
		})
	}
}

// After the cooldown, ONE probe flush is allowed through: if it is refused again
// the SDK re-suspends silently, and if it succeeds it recovers and says so once.
func TestQuotaLimitedProbeAfterCooldown(t *testing.T) {
	limited := true
	stub := startStub(func(_ capturedRequest, w http.ResponseWriter, _ int) {
		if limited {
			sendQuotaLimited(w)
			return
		}
		sendEnvelope(w, map[string]interface{}{"processed": 1}, 200)
	})
	defer stub.close()

	bd, _ := New("bd_test_x", Options{Host: stub.URL, FlushAt: 100, FlushIntervalMs: 60000})
	defer func() { _ = bd.Shutdown() }()

	logged := captureLog(func() {
		bd.Capture("u", "e", Properties{"n": 1}, nil)
		_ = bd.Flush() // suspends

		// Fast-forward past the cooldown.
		expireQuotaCooldown(bd)
		bd.Capture("u", "e", Properties{"n": 2}, nil)
		if bd.QueueLength() != 1 {
			t.Fatalf("queue length = %d, want 1 (capture accepted once the cooldown elapsed)", bd.QueueLength())
		}
		if err := bd.Flush(); !errors.Is(err, ErrQuotaLimited) { // probe, refused again
			t.Fatalf("probe error = %v, want ErrQuotaLimited", err)
		}

		// Cooldown re-armed: dark again, no second probe.
		bd.Capture("u", "e", Properties{"n": 3}, nil)
		_ = bd.Flush()
		if got := len(stub.requestsForPath("/ingest-events")); got != 2 {
			t.Fatalf("ingest requests = %d, want 2 (one initial + one probe)", got)
		}

		// Allowance comes back; the next probe recovers the pipeline.
		limited = false
		expireQuotaCooldown(bd)
		bd.Capture("u", "e", Properties{"n": 4}, nil)
		if err := bd.Flush(); err != nil {
			t.Fatalf("recovery flush error = %v, want nil", err)
		}
	})

	if n := strings.Count(logged, quotaLimitedMessage); n != 1 {
		t.Errorf("reason logged %d time(s), want exactly 1 (a failed probe must stay quiet):\n%s", n, logged)
	}
	if n := strings.Count(logged, "quota allowance restored"); n != 1 {
		t.Errorf("recovery logged %d time(s), want exactly 1:\n%s", n, logged)
	}
	if got := len(stub.requestsForPath("/ingest-events")); got != 3 {
		t.Errorf("ingest requests = %d, want 3", got)
	}
	// Recovered: capture and flush are live again.
	bd.Capture("u", "e", Properties{"n": 5}, nil)
	if bd.QueueLength() != 1 {
		t.Errorf("queue length = %d, want 1 (capture live again after recovery)", bd.QueueLength())
	}
}

// expireQuotaCooldown backdates the suspension so the next flush is the probe,
// rather than making the test sleep for quotaSuspendCooldown.
func expireQuotaCooldown(bd *Client) {
	a := bd.analytics
	a.quotaMu.Lock()
	a.quotaSince = time.Now().Add(-quotaSuspendCooldown - time.Minute)
	a.quotaMu.Unlock()
}

func TestQuotaLimitedMetricsAndReason(t *testing.T) {
	body := map[string]interface{}{
		"quota_limited": []interface{}{"events", "replays"},
		"message":       quotaLimitedMessage,
	}
	if got := quotaLimitedMetrics(body); len(got) != 2 || got[0] != "events" || got[1] != "replays" {
		t.Errorf("metrics = %v, want [events replays]", got)
	}
	if got := quotaLimitedReason(body); got != quotaLimitedMessage {
		t.Errorf("reason = %q, want the server's own message", got)
	}

	// No prose: name the meters rather than going quiet for an unstated reason.
	noMessage := map[string]interface{}{"quota_limited": []interface{}{"events"}}
	if got := quotaLimitedReason(noMessage); !strings.Contains(got, "events") {
		t.Errorf("fallback reason = %q, want it to name the drained meter", got)
	}

	for _, bad := range []interface{}{nil, "quota_limited", 42, map[string]interface{}{}, map[string]interface{}{"quota_limited": nil}} {
		if got := quotaLimitedMetrics(bad); got != nil {
			t.Errorf("metrics(%v) = %v, want nil", bad, got)
		}
		if got := quotaLimitedReason(bad); got == "" {
			t.Errorf("reason(%v) is empty, want a fallback line", bad)
		}
	}
}
