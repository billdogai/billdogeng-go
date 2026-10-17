package billdogeng

import (
	"context"
	"testing"
)

// vector is one canonical murmurhash3 test vector.
type vector struct {
	input  string
	hash   uint32
	mod100 int
}

// Spec test #3 (part A): the 12 canonical murmurhash3 vectors. These are
// cross-checked against the JS / iOS / Android implementations — they MUST all
// match exactly, otherwise local flag bucketing diverges across platforms.
var vectors = []vector{
	{"", 0, 0},
	{"a", 1009084850, 50},
	{"ab", 2613040991, 91},
	{"abc", 3017643002, 2},
	{"abcd", 1139631978, 78},
	{"hello", 613153351, 51},
	{"feature_x.user-123", 1651817416, 16},
	{"new_checkout.user-42", 2069292701, 1},
	// NOTE: these pin the raw hash function, not the flag key format. The CANONICAL flag keys are
	// "flag:{key}:{user}" and "flag-variant:{key}:{user}" — see tests/fixtures/flag-bucketing.json.
	{"dark_mode.alice", 325027212, 12},
	{"beta.bob", 3544697655, 55},
	{"my_flag.00000000-0000-0000-0000-000000000000", 2457227110, 10},
	{"flag.user_with_a_fairly_long_identifier_string_1234567890", 1645539598, 98},
}

func TestMurmurhash3CanonicalVectors(t *testing.T) {
	if len(vectors) != 12 {
		t.Fatalf("expected 12 vectors, have %d", len(vectors))
	}
	for _, v := range vectors {
		got := Murmurhash3(v.input)
		if got != v.hash {
			t.Errorf("Murmurhash3(%q) = %d, want %d", v.input, got, v.hash)
		}
		if int(got%100) != v.mod100 {
			t.Errorf("Murmurhash3(%q)%%100 = %d, want %d", v.input, got%100, v.mod100)
		}
	}
}

// Spec test #3 (part B): rollout / targeting / multivariate local evaluation.

func newLocalClient(t *testing.T) *Client {
	t.Helper()
	// These are pure bucketing / eval tests against the DEFAULT (real) host with no
	// stub. Disable `$feature_flag_called` exposure events so a flag read never
	// queues telemetry that Shutdown would then try to POST to the live API.
	off := false
	bd, err := New("bd_test_x", Options{LocalEvaluation: true, SendFeatureFlagEvents: &off})
	if err != nil {
		t.Fatal(err)
	}
	return bd
}

func TestRolloutGating(t *testing.T) {
	ctx := context.Background()
	bd := newLocalClient(t)
	defer bd.Shutdown()
	// CANONICAL buckets (murmur("flag:{key}:{user}") % 100, verified against
	// tests/fixtures/flag-bucketing.json): dark_mode/alice = 82, off_flag/alice = 86.
	bd.Flags.SetDefinitions([]FeatureFlagDefinition{
		{Key: "dark_mode", Active: true, RolloutPercentage: 83}, // 82 < 83 -> ON
		{Key: "off_flag", Active: true, RolloutPercentage: 86},  // 86 < 86 is false -> OFF
	})
	if v, _ := bd.GetFeatureFlag(ctx, "dark_mode", "alice", nil); v != true {
		t.Errorf("dark_mode = %v, want true", v)
	}
	if v, _ := bd.GetFeatureFlag(ctx, "off_flag", "alice", nil); v != false {
		t.Errorf("off_flag = %v, want false", v)
	}
}

func TestInactiveFlag(t *testing.T) {
	ctx := context.Background()
	bd := newLocalClient(t)
	defer bd.Shutdown()
	bd.Flags.SetDefinitions([]FeatureFlagDefinition{
		{Key: "dark_mode", Active: false, RolloutPercentage: 100},
	})
	if v, _ := bd.GetFeatureFlag(ctx, "dark_mode", "alice", nil); v != false {
		t.Errorf("inactive flag = %v, want false", v)
	}
}

func TestUnknownFlagNil(t *testing.T) {
	ctx := context.Background()
	bd := newLocalClient(t)
	defer bd.Shutdown()
	bd.Flags.SetDefinitions([]FeatureFlagDefinition{})
	if v, _ := bd.GetFeatureFlag(ctx, "nope", "alice", nil); v != nil {
		t.Errorf("unknown flag = %v, want nil", v)
	}
}

// TestTargetingRuleAllMustMatch uses the ONE canonical audience DSL (an AND rule with two conditions).
// The flat {attribute, operator, value} array this test used to build is GONE — it was a second condition
// DSL that could not express OR, date windows, rule status or group membership.
func TestTargetingRuleAllMustMatch(t *testing.T) {
	ctx := context.Background()
	bd := newLocalClient(t)
	defer bd.Shutdown()
	enabled := true
	bd.Flags.SetDefinitions([]FeatureFlagDefinition{
		{
			Key: "beta", Active: true, RolloutPercentage: 100,
			TargetingRule: &AudienceRule{
				Status:  "active",
				Enabled: &enabled,
				Conditions: &AudienceConditions{
					Logic: "AND",
					Rules: []AudienceCondition{
						{Type: "attribute", Field: "plan", Operator: "equals", Value: "pro"},
						{Type: "attribute", Field: "age", Operator: "greater_than", Value: 18},
					},
				},
			},
		},
	})
	// beta.bob -> bucket 55, within rollout 100.
	if v, _ := bd.GetFeatureFlag(ctx, "beta", "bob", &FlagEvalOptions{
		PersonProperties: Properties{"plan": "pro", "age": 30},
	}); v != true {
		t.Errorf("all match = %v, want true", v)
	}
	if v, _ := bd.GetFeatureFlag(ctx, "beta", "bob", &FlagEvalOptions{
		PersonProperties: Properties{"plan": "free", "age": 30},
	}); v != false {
		t.Errorf("plan mismatch = %v, want false", v)
	}
	if v, _ := bd.GetFeatureFlag(ctx, "beta", "bob", &FlagEvalOptions{
		PersonProperties: Properties{"plan": "pro", "age": 15},
	}); v != false {
		t.Errorf("age mismatch = %v, want false", v)
	}
}

// TestTargetingRuleOrAndGroups covers what the flat shape could NOT express: an OR combinator and group
// membership — both now resolved locally, with no server round-trip.
func TestTargetingRuleOrAndGroups(t *testing.T) {
	ctx := context.Background()
	bd := newLocalClient(t)
	defer bd.Shutdown()
	bd.Flags.SetDefinitions([]FeatureFlagDefinition{
		{
			Key: "beta", Active: true, RolloutPercentage: 100,
			TargetingRule: &AudienceRule{
				Conditions: &AudienceConditions{
					Logic: "OR",
					Rules: []AudienceCondition{
						{Type: "group", GroupType: "organisation", Operator: "is", Value: "acme"},
						{Type: "country", Operator: "is", Value: "JP"},
					},
				},
			},
		},
	})
	if v, _ := bd.GetFeatureFlag(ctx, "beta", "bob", &FlagEvalOptions{
		Groups: Groups{"organisation": "acme"},
	}); v != true {
		t.Errorf("group hit = %v, want true", v)
	}
	if v, _ := bd.GetFeatureFlag(ctx, "beta", "bob", &FlagEvalOptions{
		Groups: Groups{"organisation": "globex"}, Country: "JP",
	}); v != true {
		t.Errorf("country hit (OR) = %v, want true", v)
	}
	if v, _ := bd.GetFeatureFlag(ctx, "beta", "bob", &FlagEvalOptions{
		Groups: Groups{"organisation": "globex"}, Country: "US",
	}); v != false {
		t.Errorf("neither arm = %v, want false", v)
	}
	// country falls back to a same-named person property when the explicit field is unset.
	if v, _ := bd.GetFeatureFlag(ctx, "beta", "bob", &FlagEvalOptions{
		PersonProperties: Properties{"country": "JP"},
	}); v != true {
		t.Errorf("country from person properties = %v, want true", v)
	}
}

func TestMultivariate(t *testing.T) {
	ctx := context.Background()
	bd := newLocalClient(t)
	defer bd.Shutdown()
	// Variant selection uses an INDEPENDENT hash — murmur("flag-variant:{key}:{user}") % 100 — NOT the
	// rollout bucket. beta/alice: rollout=88 (< 100, so ON), variant=36 -> "test" in [30,70).
	// (Reusing the rollout bucket was the starvation bug: it made variant B unreachable at low rollouts.)
	bd.Flags.SetDefinitions([]FeatureFlagDefinition{
		{
			Key: "beta", Active: true, RolloutPercentage: 100,
			Variants: []FlagVariant{
				{Key: "control", RolloutPercentage: 30, Payload: map[string]interface{}{"color": "blue"}},
				{Key: "test", RolloutPercentage: 40, Payload: map[string]interface{}{"color": "green"}},
				{Key: "big", RolloutPercentage: 30},
			},
		},
	})
	if v, _ := bd.GetFeatureFlag(ctx, "beta", "alice", nil); v != "test" {
		t.Errorf("variant = %v, want test", v)
	}
	payload, _ := bd.GetFeatureFlagPayload(ctx, "beta", "alice", nil)
	m, ok := payload.(map[string]interface{})
	if !ok || m["color"] != "green" {
		t.Errorf("payload = %v, want {color: green}", payload)
	}
}

func TestGetAllFlags(t *testing.T) {
	ctx := context.Background()
	bd := newLocalClient(t)
	defer bd.Shutdown()
	bd.Flags.SetDefinitions([]FeatureFlagDefinition{
		{Key: "dark_mode", Active: true, RolloutPercentage: 83}, // alice bucket 82 -> ON
		{Key: "never", Active: true, RolloutPercentage: 0},
	})
	all, _ := bd.GetAllFlags(ctx, "alice", nil)
	if all["dark_mode"] != true {
		t.Errorf("dark_mode = %v, want true", all["dark_mode"])
	}
	if all["never"] != false {
		t.Errorf("never = %v, want false", all["never"])
	}
}
