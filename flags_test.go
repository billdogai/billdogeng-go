package billdogeng

import "testing"

// TestCompareCanonicalVectors asserts the Go Compare implementation (targeting.go — the SDK's ONE
// operator matcher) matches the backend audience engine compare() in
// supabase/functions/_shared/edge-serving/pure.ts for every canonical cross-language vector in
// scratch/sdk-operator-parity-spec.md.
func TestCompareCanonicalVectors(t *testing.T) {
	cases := []struct {
		name     string
		operator string
		actual   interface{}
		expected interface{}
		want     bool
	}{
		{"exists present", "exists", "x", nil, true},
		{"exists empty", "exists", "", nil, false},
		{"not_exists null", "not_exists", nil, nil, true},
		{"equals", "equals", "5", "5", true},
		{"is", "is", "5", "5", true},
		{"not_equals", "not_equals", "5", "6", true},
		{"is_not same", "is_not", "a", "a", false},
		{"any_of hit", "any_of", "b", []interface{}{"a", "b", "c"}, true},
		{"any_of miss", "any_of", "z", []interface{}{"a", "b", "c"}, false},
		{"not_any_of miss", "not_any_of", "z", []interface{}{"a", "b", "c"}, true},
		{"contains", "contains", "hello", "ell", true},
		{"not_contains", "not_contains", "hello", "xyz", true},
		{"greater_than numeric", "greater_than", "10", "5", true},
		{"gt alias numeric", "gt", "3", "5", false},
		{"less_than numeric", "less_than", "3", "5", true},
		{"gte numeric equal", "gte", "5", "5", true},
		{"lte numeric equal", "lte", "5", "5", true},
		{"greater_than ISO date", "greater_than", "2026-02-01", "2026-01-01", true},
		{"less_than ISO date", "less_than", "2026-01-01", "2026-02-01", true},
		{"gte ISO date equal", "gte", "2026-01-01", "2026-01-01", true},
		// An ABSENT actual short-circuits to false for every operator except not_exists — it must never
		// read as "not equal to X" / "not one of X", which would hand a targeted flag to a user we know
		// nothing about.
		{"absent is_not", "is_not", nil, "a", false},
		{"absent not_any_of", "not_any_of", nil, []interface{}{"a"}, false},
		// JS Number() coercion, which the backend inherits — Go's ParseFloat does NOT match it.
		// Pinned by the fixture (numeric-lt x score-empty).
		{"empty string coerces to 0 (lt)", "less_than", "", 5, true},
		{"empty string coerces to 0 (gt)", "greater_than", "", 5, false},
		{"empty string coerces to 0 (gte)", "greater_than_or_equal", "", 5, false},
		{"junk string is NaN (lt)", "less_than", "abc", 5, false},
		{"junk string is NaN (gte)", "greater_than_or_equal", "abc", 5, false},
		{"bool stringifies to 'true', which is NaN", "less_than", true, 5, false},
		{"numeric string", "greater_than", "10", 5, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Compare(tc.operator, tc.actual, tc.expected); got != tc.want {
				t.Errorf("Compare(%q, %v, %v) = %v, want %v",
					tc.operator, tc.actual, tc.expected, got, tc.want)
			}
		})
	}
}
