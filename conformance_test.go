package billdogeng

// CROSS-SIDE FLAG BUCKETING CONFORMANCE.
//
// This SDK is asserted against tests/fixtures/flag-bucketing.json — vectors GENERATED FROM THE BACKEND
// evaluator (supabase/functions/_shared/edge-serving/pure.ts). NEVER against another SDK.
//
// That distinction is the whole point. The murmur bucketing was hand-ported into node/go/python/elixir;
// all four agreed with EACH OTHER and ALL FOUR DISAGREED WITH THE SERVER, for months, because every test
// compared one SDK to another. Separately, every SDK reused the ROLLOUT bucket to pick a variant, so a
// flag at 10% rollout split 50/50 assigned variant B to NOBODY. Neither bug is visible to a test that
// does not cross the boundary.
//
// The fixture's "starvation" flag (10% rollout, 50/50 split) is the one that matters: it assigns BOTH
// variants across the user set, so a shared-bucket regression fails here loudly.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type fixtureVariant struct {
	Key               string      `json:"key"`
	RolloutPercentage float64     `json:"rollout_percentage"`
	Payload           interface{} `json:"payload,omitempty"`
}

type fixtureFlag struct {
	Key               string           `json:"key"`
	Enabled           bool             `json:"enabled"`
	RolloutPercentage float64          `json:"rollout_percentage"`
	Variants          []fixtureVariant `json:"variants,omitempty"`
}

type fixtureVector struct {
	FlagKey    string      `json:"flag_key"`
	DistinctID string      `json:"distinct_id"`
	Verdict    interface{} `json:"verdict"` // false | true | "variant-key"
}

type fixture struct {
	Flags   []fixtureFlag   `json:"flags"`
	Vectors []fixtureVector `json:"vectors"`
}

// loadFixture walks up to the repo root so the test works from any working directory.
func loadFixture(t *testing.T) fixture {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		p := filepath.Join(dir, "tests", "fixtures", "flag-bucketing.json")
		if _, err := os.Stat(p); err == nil {
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var f fixture
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatal(err)
			}
			return f
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find tests/fixtures/flag-bucketing.json walking up from cwd")
	return fixture{}
}

func TestConformsToBackendBucketingVectors(t *testing.T) {
	ctx := context.Background()
	fx := loadFixture(t)

	if len(fx.Vectors) < 100 {
		t.Fatalf("fixture has only %d vectors — too few to be meaningful", len(fx.Vectors))
	}

	defs := make([]FeatureFlagDefinition, 0, len(fx.Flags))
	for _, f := range fx.Flags {
		variants := make([]FlagVariant, 0, len(f.Variants))
		for _, v := range f.Variants {
			variants = append(variants, FlagVariant{Key: v.Key, RolloutPercentage: v.RolloutPercentage, Payload: v.Payload})
		}
		defs = append(defs, FeatureFlagDefinition{
			Key:               f.Key,
			Active:            f.Enabled, // the wire shape says `active`; the DB column is `enabled`
			RolloutPercentage: f.RolloutPercentage,
			Variants:          variants,
		})
	}

	bd := newLocalClient(t)
	defer bd.Shutdown()
	bd.Flags.SetDefinitions(defs)

	mismatches := 0
	for _, v := range fx.Vectors {
		got, err := bd.GetFeatureFlag(ctx, v.FlagKey, v.DistinctID, nil)
		if err != nil {
			t.Fatalf("%s/%s: %v", v.FlagKey, v.DistinctID, err)
		}
		if !sameVerdict(got, v.Verdict) {
			mismatches++
			if mismatches <= 5 {
				t.Errorf("%s/%s: go=%v backend=%v", v.FlagKey, v.DistinctID, got, v.Verdict)
			}
		}
	}
	if mismatches > 0 {
		t.Fatalf("%d/%d vectors disagree with the BACKEND — this SDK buckets differently than the server",
			mismatches, len(fx.Vectors))
	}
}

// TestFixtureCanDetectTheStarvationBug guards the guard: a fixture in which the 10%/50-50 flag only ever
// produced one variant could not tell correct code from the shared-bucket bug.
func TestFixtureCanDetectTheStarvationBug(t *testing.T) {
	fx := loadFixture(t)
	seen := map[string]bool{}
	for _, v := range fx.Vectors {
		if v.FlagKey != "starvation" {
			continue
		}
		if s, ok := v.Verdict.(string); ok {
			seen[s] = true
		}
	}
	if !seen["variant-a"] || !seen["variant-b"] {
		t.Fatalf("the starvation vectors do not contain BOTH variants (%v) — this fixture cannot detect "+
			"the bug it exists to catch", seen)
	}
}

func sameVerdict(got FlagValue, want interface{}) bool {
	switch w := want.(type) {
	case bool:
		b, ok := got.(bool)
		return ok && b == w
	case string:
		s, ok := got.(string)
		return ok && s == w
	}
	return false
}
