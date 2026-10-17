package billdogeng

// `$feature_flag_called` exposure events (PostHog parity) + GeoIP resolution header.
//
// A flag read must emit ONE `$feature_flag_called` analytics event per (distinctId, flag, value) — the
// exposure record experiments/analytics attribute outcomes to — deduped, gated by SendFeatureFlagEvents,
// and fire-and-forget through the SDK's own analytics capture pipeline. These mirror
// node/test/feature-flag-called.test.ts.

import (
	"context"
	"net/http"
	"sort"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// exposureStub serves one locally-evaluable flag and records /ingest-events batches.
func exposureStub() *stubServer {
	return startStub(func(req capturedRequest, w http.ResponseWriter, _ int) {
		switch req.Path {
		case "/feature-flag-definitions":
			sendEnvelope(w, map[string]interface{}{
				"flags": []map[string]interface{}{
					{"key": "plain", "active": true, "rollout_percentage": 100},
				},
			}, 200)
		case "/ingest-events":
			sendEnvelope(w, map[string]interface{}{"processed": 1}, 200)
		default:
			sendRaw(w, map[string]interface{}{}, 404)
		}
	})
}

// flagCalledEvents returns every `$feature_flag_called` event across all recorded ingest batches.
func flagCalledEvents(s *stubServer) []map[string]interface{} {
	var out []map[string]interface{}
	for _, r := range s.requestsForPath("/ingest-events") {
		evts, _ := r.Body["events"].([]interface{})
		for _, e := range evts {
			m, ok := e.(map[string]interface{})
			if !ok {
				continue
			}
			if m["event_name"] == "$feature_flag_called" {
				out = append(out, m)
			}
		}
	}
	return out
}

func TestExposureEmitsCanonicalShape(t *testing.T) {
	ctx := context.Background()
	s := exposureStub()
	defer s.close()
	bd, err := New("bd_test_x", Options{Host: s.URL, FlushAt: 1000, LocalEvaluation: true})
	if err != nil {
		t.Fatal(err)
	}

	if v, _ := bd.GetFeatureFlag(ctx, "plain", "alice", nil); v != true {
		t.Fatalf("plain = %v, want true", v)
	}
	if err := bd.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	calls := flagCalledEvents(s)
	if len(calls) != 1 {
		t.Fatalf("exposure events = %d, want 1", len(calls))
	}
	if calls[0]["user_id"] != "alice" {
		t.Errorf("user_id = %v, want alice", calls[0]["user_id"])
	}
	props, _ := calls[0]["properties"].(map[string]interface{})
	if props["$feature_flag"] != "plain" {
		t.Errorf("$feature_flag = %v, want plain", props["$feature_flag"])
	}
	if props["$feature_flag_response"] != true {
		t.Errorf("$feature_flag_response = %v, want true", props["$feature_flag_response"])
	}
	_ = bd.Shutdown()
}

func TestExposureDedupePerUserFlagValue(t *testing.T) {
	ctx := context.Background()
	s := exposureStub()
	defer s.close()
	bd, err := New("bd_test_x", Options{Host: s.URL, FlushAt: 1000, LocalEvaluation: true})
	if err != nil {
		t.Fatal(err)
	}

	_, _ = bd.GetFeatureFlag(ctx, "plain", "alice", nil)
	_, _ = bd.GetFeatureFlag(ctx, "plain", "alice", nil)   // duplicate -> suppressed
	_, _ = bd.IsFeatureEnabled(ctx, "plain", "alice", nil) // delegates -> still deduped
	_, _ = bd.GetFeatureFlag(ctx, "plain", "bob", nil)     // different user -> emitted
	if err := bd.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	calls := flagCalledEvents(s)
	if len(calls) != 2 {
		t.Fatalf("exposure events = %d, want 2 (deduped per user, not across users)", len(calls))
	}
	users := []string{}
	for _, c := range calls {
		users = append(users, c["user_id"].(string))
	}
	sort.Strings(users)
	if users[0] != "alice" || users[1] != "bob" {
		t.Errorf("user_ids = %v, want [alice bob]", users)
	}
	_ = bd.Shutdown()
}

func TestExposureGlobalSuppress(t *testing.T) {
	ctx := context.Background()
	s := exposureStub()
	defer s.close()
	bd, err := New("bd_test_x", Options{
		Host: s.URL, FlushAt: 1000, LocalEvaluation: true, SendFeatureFlagEvents: boolPtr(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = bd.GetFeatureFlag(ctx, "plain", "alice", nil)
	_ = bd.Flush()
	if n := len(flagCalledEvents(s)); n != 0 {
		t.Fatalf("exposure events = %d, want 0 (global suppress)", n)
	}
	_ = bd.Shutdown()
}

func TestExposurePerCallSuppress(t *testing.T) {
	ctx := context.Background()
	s := exposureStub()
	defer s.close()
	bd, err := New("bd_test_x", Options{Host: s.URL, FlushAt: 1000, LocalEvaluation: true})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = bd.GetFeatureFlag(ctx, "plain", "alice", &FlagEvalOptions{SendFeatureFlagEvents: boolPtr(false)})
	_ = bd.Flush()
	if n := len(flagCalledEvents(s)); n != 0 {
		t.Fatalf("exposure events = %d, want 0 (per-call suppress)", n)
	}
	_ = bd.Shutdown()
}

func TestGeoipHeaderPresentWhenConfigured(t *testing.T) {
	s := exposureStub()
	defer s.close()
	bd, err := New("bd_test_x", Options{Host: s.URL, FlushAt: 1000, GeoipResolutionLevel: "none"})
	if err != nil {
		t.Fatal(err)
	}
	bd.Capture("alice", "evt", nil, nil)
	_ = bd.Flush()
	ingest := s.requestsForPath("/ingest-events")
	if len(ingest) != 1 {
		t.Fatalf("ingest requests = %d, want 1", len(ingest))
	}
	if got := ingest[0].Headers.Get("x-geoip-resolution-level"); got != "none" {
		t.Errorf("x-geoip-resolution-level = %q, want none", got)
	}
	_ = bd.Shutdown()
}

func TestGeoipHeaderOmittedWhenUnset(t *testing.T) {
	s := exposureStub()
	defer s.close()
	bd, err := New("bd_test_x", Options{Host: s.URL, FlushAt: 1000})
	if err != nil {
		t.Fatal(err)
	}
	bd.Capture("alice", "evt", nil, nil)
	_ = bd.Flush()
	ingest := s.requestsForPath("/ingest-events")
	if len(ingest) != 1 {
		t.Fatalf("ingest requests = %d, want 1", len(ingest))
	}
	if got := ingest[0].Headers.Get("x-geoip-resolution-level"); got != "" {
		t.Errorf("x-geoip-resolution-level = %q, want empty (header omitted)", got)
	}
	_ = bd.Shutdown()
}
