package billdogeng

// Tier-2 PostHog-parity features, mirroring node/test/tier2-features.test.ts:
// GetAllFlagsAndPayloads, Capture Timestamp override, SendFeatureFlags ($feature/ injection),
// OnlyEvaluateLocally, Disabled kill-switch, BeforeSend hook, and FeatureFlagsPollingInterval
// (configurable definitions TTL).

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"
)

// ingested flattens every event across all /ingest-events batches recorded by the stub.
func ingested(s *stubServer) []map[string]interface{} {
	var out []map[string]interface{}
	for _, r := range s.requestsForPath("/ingest-events") {
		evts, _ := r.Body["events"].([]interface{})
		for _, e := range evts {
			if m, ok := e.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

func findEvent(evts []map[string]interface{}, name string) map[string]interface{} {
	for _, e := range evts {
		if e["event_name"] == name {
			return e
		}
	}
	return nil
}

func ingestOnlyStub() *stubServer {
	return startStub(func(req capturedRequest, w http.ResponseWriter, _ int) {
		if req.Path == "/ingest-events" {
			sendEnvelope(w, map[string]interface{}{}, 200)
			return
		}
		sendRaw(w, map[string]interface{}{}, 404)
	})
}

// #1 getAllFlagsAndPayloads: returns {value, payload} per flag in local mode.
func TestGetAllFlagsAndPayloads(t *testing.T) {
	ctx := context.Background()
	bd, err := New("bd_test_x", Options{LocalEvaluation: true, SendFeatureFlagEvents: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	bd.Flags.SetDefinitions([]FeatureFlagDefinition{
		{Key: "plain", Active: true, RolloutPercentage: 100, Payload: map[string]interface{}{"color": "blue"}},
		{Key: "off", Active: true, RolloutPercentage: 0},
	})
	all, err := bd.GetAllFlagsAndPayloads(ctx, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if all["plain"].Value != true {
		t.Errorf("plain.value = %v, want true", all["plain"].Value)
	}
	if !reflect.DeepEqual(all["plain"].Payload, map[string]interface{}{"color": "blue"}) {
		t.Errorf("plain.payload = %v, want {color:blue}", all["plain"].Payload)
	}
	if all["off"].Value != false {
		t.Errorf("off.value = %v, want false", all["off"].Value)
	}
	if all["off"].Payload != nil {
		t.Errorf("off.payload = %v, want nil", all["off"].Payload)
	}
	_ = bd.Shutdown()
}

// #2 capture Timestamp override: uses the caller-supplied timestamp (epoch ms and time.Time).
func TestCaptureTimestampOverride(t *testing.T) {
	s := ingestOnlyStub()
	defer s.close()
	bd, err := New("bd_test_x", Options{Host: s.URL, FlushAt: 1000})
	if err != nil {
		t.Fatal(err)
	}
	bd.CaptureWithOptions("alice", "e1", nil, nil, CaptureOptions{Timestamp: time.UnixMilli(1000)})
	bd.CaptureWithOptions("bob", "e2", nil, nil, CaptureOptions{Timestamp: time.UnixMilli(2000)})
	if err := bd.Flush(); err != nil {
		t.Fatal(err)
	}
	evts := ingested(s)
	if ts := findEvent(evts, "e1")["event_timestamp"]; ts != float64(1000) {
		t.Errorf("e1 timestamp = %v, want 1000", ts)
	}
	if ts := findEvent(evts, "e2")["event_timestamp"]; ts != float64(2000) {
		t.Errorf("e2 timestamp = %v, want 2000", ts)
	}
	_ = bd.Shutdown()
}

// Capture (no options) still stamps "now" — a zero Timestamp must not become epoch 0.
func TestCaptureDefaultTimestampIsNow(t *testing.T) {
	s := ingestOnlyStub()
	defer s.close()
	bd, _ := New("bd_test_x", Options{Host: s.URL, FlushAt: 1000})
	before := time.Now().UnixMilli()
	bd.Capture("alice", "e1", nil, nil)
	_ = bd.Flush()
	ts, _ := findEvent(ingested(s), "e1")["event_timestamp"].(float64)
	if int64(ts) < before {
		t.Errorf("default timestamp = %v, want >= %d (now)", ts, before)
	}
	_ = bd.Shutdown()
}

// #2 sendFeatureFlags injection: attaches $feature/<key> and $active_feature_flags.
func TestSendFeatureFlagsInjection(t *testing.T) {
	s := ingestOnlyStub()
	defer s.close()
	bd, err := New("bd_test_x", Options{
		Host: s.URL, FlushAt: 1000, LocalEvaluation: true, SendFeatureFlagEvents: boolPtr(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	bd.Flags.SetDefinitions([]FeatureFlagDefinition{
		{Key: "on", Active: true, RolloutPercentage: 100},
		{Key: "off", Active: true, RolloutPercentage: 0},
	})
	bd.CaptureWithOptions("alice", "purchase", nil, nil, CaptureOptions{SendFeatureFlags: true})
	if err := bd.Flush(); err != nil {
		t.Fatal(err)
	}
	evt := findEvent(ingested(s), "purchase")
	props, _ := evt["properties"].(map[string]interface{})
	if props["$feature/on"] != true {
		t.Errorf("$feature/on = %v, want true", props["$feature/on"])
	}
	if props["$feature/off"] != false {
		t.Errorf("$feature/off = %v, want false", props["$feature/off"])
	}
	active, _ := props["$active_feature_flags"].([]interface{})
	if len(active) != 1 || active[0] != "on" {
		t.Errorf("$active_feature_flags = %v, want [on]", props["$active_feature_flags"])
	}
	_ = bd.Shutdown()
}

// #3 onlyEvaluateLocally: returns nil for a server-only flag and never calls the server.
func TestOnlyEvaluateLocally(t *testing.T) {
	ctx := context.Background()
	s := startStub(func(req capturedRequest, w http.ResponseWriter, _ int) {
		switch req.Path {
		case "/feature-flag-definitions":
			sendEnvelope(w, map[string]interface{}{
				"flags": []map[string]interface{}{
					{"key": "seg", "active": true, "rollout_percentage": 100, "requires_server_evaluation": true},
				},
			}, 200)
		case "/ingest-events":
			sendEnvelope(w, map[string]interface{}{}, 200)
		default:
			sendRaw(w, map[string]interface{}{}, 404)
		}
	})
	defer s.close()
	bd, err := New("bd_test_x", Options{Host: s.URL, FlushAt: 1000, LocalEvaluation: true, SendFeatureFlagEvents: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	v, err := bd.GetFeatureFlag(ctx, "seg", "alice", &FlagEvalOptions{OnlyEvaluateLocally: true})
	if err != nil {
		t.Fatal(err)
	}
	if v != nil {
		t.Errorf("seg (onlyEvaluateLocally) = %v, want nil", v)
	}
	if len(s.requestsForPath("/experiment-config")) != 0 {
		t.Errorf("server was called, want no /experiment-config request")
	}
	_ = bd.Shutdown()
}

// #4 disabled kill-switch: no-ops capture and flag reads with zero network.
func TestDisabledKillSwitch(t *testing.T) {
	ctx := context.Background()
	// Any hit errors: proves nothing goes out.
	s := startStub(func(_ capturedRequest, w http.ResponseWriter, _ int) {
		sendRaw(w, map[string]interface{}{}, 500)
	})
	defer s.close()
	bd, err := New("bd_test_x", Options{Host: s.URL, Disabled: true, LocalEvaluation: true})
	if err != nil {
		t.Fatal(err)
	}
	bd.Capture("alice", "e1", nil, nil)
	bd.Identify("alice", Properties{"x": 1})
	bd.SetPersonProperties("alice", PersonPropertyOps{Set: Properties{"y": 2}})
	if n := bd.QueueLength(); n != 0 {
		t.Errorf("queue length = %d, want 0", n)
	}
	if v, _ := bd.GetFeatureFlag(ctx, "any", "alice", nil); v != nil {
		t.Errorf("GetFeatureFlag = %v, want nil", v)
	}
	all, _ := bd.GetAllFlags(ctx, "alice", nil)
	if len(all) != 0 {
		t.Errorf("GetAllFlags = %v, want empty", all)
	}
	_ = bd.Flush()
	time.Sleep(50 * time.Millisecond) // let any stray fire-and-forget goroutine run
	if n := len(s.requests); n != 0 {
		t.Errorf("network requests = %d, want 0 (SDK disabled)", n)
	}
	_ = bd.Shutdown()
}

// #5 beforeSend hook: mutates and drops events.
func TestBeforeSendHook(t *testing.T) {
	s := ingestOnlyStub()
	defer s.close()
	bd, err := New("bd_test_x", Options{
		Host: s.URL, FlushAt: 1000,
		BeforeSend: func(e WireEvent) *WireEvent {
			if e.EventName == "secret" {
				return nil // drop
			}
			e.Properties["tagged"] = true // mutate
			return &e
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	bd.Capture("alice", "keep", nil, nil)
	bd.Capture("alice", "secret", nil, nil)
	if err := bd.Flush(); err != nil {
		t.Fatal(err)
	}
	evts := ingested(s)
	if len(evts) != 1 || evts[0]["event_name"] != "keep" {
		t.Fatalf("events = %v, want only [keep]", evts)
	}
	props, _ := evts[0]["properties"].(map[string]interface{})
	if props["tagged"] != true {
		t.Errorf("tagged = %v, want true (mutation applied)", props["tagged"])
	}
	_ = bd.Shutdown()
}

// #6 featureFlagsPollingInterval: refetches definitions once the TTL elapses.
func TestFeatureFlagsPollingInterval(t *testing.T) {
	ctx := context.Background()
	s := startStub(func(req capturedRequest, w http.ResponseWriter, _ int) {
		switch req.Path {
		case "/feature-flag-definitions":
			sendEnvelope(w, map[string]interface{}{
				"flags": []map[string]interface{}{{"key": "plain", "active": true, "rollout_percentage": 100}},
			}, 200)
		case "/ingest-events":
			sendEnvelope(w, map[string]interface{}{}, 200)
		default:
			sendRaw(w, map[string]interface{}{}, 404)
		}
	})
	defer s.close()
	bd, err := New("bd_test_x", Options{
		Host: s.URL, FlushAt: 1000, LocalEvaluation: true, SendFeatureFlagEvents: boolPtr(false),
		FeatureFlagsPollingInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bd.GetFeatureFlag(ctx, "plain", "alice", nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(35 * time.Millisecond) // let the TTL lapse
	if _, err := bd.GetFeatureFlag(ctx, "plain", "bob", nil); err != nil {
		t.Fatal(err)
	}
	if n := len(s.requestsForPath("/feature-flag-definitions")); n < 2 {
		t.Errorf("/feature-flag-definitions requests = %d, want >= 2 (TTL refetch)", n)
	}
	_ = bd.Shutdown()
}
