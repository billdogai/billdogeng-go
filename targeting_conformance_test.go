package billdogeng

// CANONICAL TARGETING CONFORMANCE.
//
// Pinned to tests/fixtures/flag-targeting.json — vectors GENERATED FROM the backend evaluator
// (evaluateTargetingRulePure in supabase/functions/_shared/edge-serving/pure.ts). NEVER against another
// SDK. Nine hand-ports of the murmur hash once all agreed with each other and all disagreed with the
// server, for months, because every SDK's tests were written against a sibling SDK. Local rule evaluation
// is a far bigger surface than a hash, so it gets the same discipline from day one.
//
// What this proves, beyond "the operators work":
//   - group MEMBERSHIP resolves locally (it was round-tripped to the server before Phase 3),
//   - an OR rule is not silently ANDed,
//   - a paused / expired / not-yet-started rule withholds the flag,
//   - legacy `segment_ids` fail CLOSED rather than being ignored,
//   - DB-backed conditions are never resolved locally — they are refused, and the flag goes to the server.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

type targetingNamedRule struct {
	Name string       `json:"name"`
	Rule AudienceRule `json:"rule"`
}

type targetingNamedContext struct {
	Name string          `json:"name"`
	Ctx  AudienceContext `json:"ctx"`
}

type targetingVector struct {
	RuleName    string `json:"rule_name"`
	ContextName string `json:"context_name"`
	Matched     bool   `json:"matched"`
}

type targetingFixture struct {
	Rules           []targetingNamedRule    `json:"rules"`
	ServerOnlyRules []targetingNamedRule    `json:"server_only_rules"`
	Contexts        []targetingNamedContext `json:"contexts"`
	Vectors         []targetingVector       `json:"vectors"`
}

// loadTargetingFixture walks up to the repo root so the test works from any working directory. It never
// skips — a missing fixture is a failure.
func loadTargetingFixture(t *testing.T) targetingFixture {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		p := filepath.Join(dir, "tests", "fixtures", "flag-targeting.json")
		if _, err := os.Stat(p); err == nil {
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var fx targetingFixture
			if err := json.Unmarshal(raw, &fx); err != nil {
				t.Fatal(err)
			}
			return fx
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find tests/fixtures/flag-targeting.json walking up from cwd")
	return targetingFixture{}
}

func targetingRuleIndex(fx targetingFixture) map[string]AudienceRule {
	m := make(map[string]AudienceRule, len(fx.Rules))
	for _, r := range fx.Rules {
		m[r.Name] = r.Rule
	}
	return m
}

func targetingContextIndex(fx targetingFixture) map[string]AudienceContext {
	m := make(map[string]AudienceContext, len(fx.Contexts))
	for _, c := range fx.Contexts {
		m[c.Name] = c.Ctx
	}
	return m
}

func TestTargetingConformsToBackendVectors(t *testing.T) {
	fx := loadTargetingFixture(t)
	if len(fx.Vectors) < 100 {
		t.Fatalf("fixture has only %d vectors — too few to be meaningful", len(fx.Vectors))
	}
	rules := targetingRuleIndex(fx)
	contexts := targetingContextIndex(fx)

	mismatches := 0
	for _, v := range fx.Vectors {
		rule, ok := rules[v.RuleName]
		if !ok {
			t.Fatalf("vector references unknown rule %q", v.RuleName)
		}
		ctx, ok := contexts[v.ContextName]
		if !ok {
			t.Fatalf("vector references unknown context %q", v.ContextName)
		}
		matched, cold := EvaluateTargetingRule(rule, ctx)
		switch {
		case cold:
			mismatches++
			if mismatches <= 8 {
				t.Errorf("%s x %s: went COLD, backend resolved it purely", v.RuleName, v.ContextName)
			}
		case matched != v.Matched:
			mismatches++
			if mismatches <= 8 {
				t.Errorf("%s x %s: go=%v backend=%v", v.RuleName, v.ContextName, matched, v.Matched)
			}
		}
	}
	if mismatches > 0 {
		t.Fatalf("%d/%d vectors disagree with the BACKEND — this SDK targets differently than the server",
			mismatches, len(fx.Vectors))
	}
}

// TestTargetingFixtureIsDiscriminating guards the guard: a fixture whose every vector is `false` would be
// satisfied by an evaluator that always returns false. It would certify a completely broken SDK.
func TestTargetingFixtureIsDiscriminating(t *testing.T) {
	fx := loadTargetingFixture(t)
	matched := 0
	for _, v := range fx.Vectors {
		if v.Matched {
			matched++
		}
	}
	if matched <= 10 || matched >= len(fx.Vectors)-10 {
		t.Fatalf("fixture is not discriminating: %d/%d vectors matched", matched, len(fx.Vectors))
	}
}

// TestTargetingRefusesDBBackedConditions asserts the COLD contract. The backend marks these flags
// requires_server_evaluation, so they should never reach the local evaluator. If one ever does it must go
// COLD — never resolve to a verdict. Resolving a `segment` condition to false would silently withhold the
// flag; resolving it to true would hand a targeted feature to everyone.
func TestTargetingRefusesDBBackedConditions(t *testing.T) {
	fx := loadTargetingFixture(t)
	if len(fx.ServerOnlyRules) == 0 {
		t.Fatal("fixture has no server_only_rules — the COLD contract is untested")
	}
	ctx := targetingContextIndex(fx)["us-ios-acme-pro"]

	for _, r := range fx.ServerOnlyRules {
		hasCold := false
		if r.Rule.Conditions != nil {
			for _, c := range r.Rule.Conditions.Rules {
				if IsColdConditionType(c.Type) {
					hasCold = true
				}
			}
		}
		if !hasCold {
			t.Errorf("%s is listed server-only but has no DB-backed condition", r.Name)
			continue
		}
		// Against a context that satisfies every OTHER condition, the DB-backed one is load-bearing.
		if _, cold := EvaluateTargetingRule(r.Rule, ctx); !cold {
			t.Errorf("%s resolved locally instead of deferring to the server", r.Name)
		}
	}
}

// TestGroupMembershipResolvesLocally — the whole point of Phase 3. Before it, this required a server
// round-trip, not because the data was missing (the SDK holds the groups map) but because the flat wire
// shape had nowhere to put group_type.
func TestGroupMembershipResolvesLocally(t *testing.T) {
	fx := loadTargetingFixture(t)
	rule, ok := targetingRuleIndex(fx)["group-org-is"]
	if !ok {
		t.Fatal("fixture is missing the group-org-is rule")
	}
	matched, cold := EvaluateTargetingRule(rule, AudienceContext{Groups: Groups{"organisation": "acme"}})
	if cold || !matched {
		t.Errorf("acme: matched=%v cold=%v, want matched=true cold=false", matched, cold)
	}
	if matched, _ := EvaluateTargetingRule(rule, AudienceContext{Groups: Groups{"organisation": "globex"}}); matched {
		t.Error("globex matched a rule targeting acme")
	}
}

// TestColdFlagFallsBackToServer proves the COLD verdict is not collapsed into false at the flag layer: a
// locally-held definition carrying a DB-backed condition must be resolved by the SERVER, whose verdict
// wins.
func TestColdFlagFallsBackToServer(t *testing.T) {
	stub := startStub(func(req capturedRequest, w http.ResponseWriter, hits int) {
		sendEnvelope(w, map[string]interface{}{
			"feature_flags": map[string]interface{}{"cohort_flag": true},
		}, 200)
	})
	defer stub.close()

	bd, err := New("bd_test_x", Options{Host: stub.URL, LocalEvaluation: true})
	if err != nil {
		t.Fatal(err)
	}
	defer bd.Shutdown()

	// requires_server_evaluation is deliberately FALSE: this is the belt-and-braces path, where the server
	// believed the flag was locally evaluable but a DB-backed condition turns out to be load-bearing.
	bd.Flags.SetDefinitions([]FeatureFlagDefinition{{
		Key: "cohort_flag", Active: true, RolloutPercentage: 100,
		TargetingRule: &AudienceRule{
			Conditions: &AudienceConditions{Logic: "AND", Rules: []AudienceCondition{
				{Type: "cohort", Operator: "is", Value: "coh-1"},
			}},
		},
	}})

	if _, cold := bd.Flags.EvaluateLocally("cohort_flag", "alice", nil); !cold {
		t.Fatal("a cohort condition resolved locally instead of going COLD")
	}

	ctx := context.Background()
	v, err := bd.GetFeatureFlag(ctx, "cohort_flag", "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if v != true {
		t.Errorf("cold flag = %v, want the server's verdict (true)", v)
	}
	if len(stub.requestsForPath("/experiment-config")) != 1 {
		t.Errorf("expected exactly ONE /experiment-config call, got %d",
			len(stub.requestsForPath("/experiment-config")))
	}

	// GetAllFlags must batch the same fallback into ONE request (2 total for this test).
	all, err := bd.GetAllFlags(ctx, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if all["cohort_flag"] != true {
		t.Errorf("GetAllFlags[cohort_flag] = %v, want true", all["cohort_flag"])
	}
	if len(stub.requestsForPath("/experiment-config")) != 2 {
		t.Errorf("GetAllFlags did not batch: %d total /experiment-config calls, want 2",
			len(stub.requestsForPath("/experiment-config")))
	}
}
