package billdogeng

// targeting.go — the canonical audience-rule evaluator, ported from the backend.
//
// This is a port of `evaluateTargetingRulePure` + `compare` in
// supabase/functions/_shared/edge-serving/pure.ts. It is pinned to tests/fixtures/flag-targeting.json,
// which is GENERATED FROM that backend evaluator. It is never checked against another SDK — nine hand-ports
// of the murmur hash once all agreed with each other and all disagreed with the server, for months,
// precisely because each SDK's tests were written against a sibling SDK.
//
// WHY THIS EXISTS. The SDK previously received a flat {attribute, operator, value} array, which could
// express none of: an OR combinator, a date window, a paused rule, group membership, or most of the
// operator set. Every flag using one of those was deferred to the server even though the SDK held
// everything needed to answer it. That flat shape was also a SECOND condition DSL, duplicated across nine
// SDKs. It is gone: the SDK now receives the rule verbatim in the one canonical DSL and resolves it here.
//
// THE COLD CONTRACT. Some condition types (segment, cohort, survey_answer, event_fired_in_window,
// group_property) are answerable only by querying the database. We cannot resolve them and we must not
// guess: a failed lookup silently reading as "no match" is how targeting breaks quietly. Those conditions
// go COLD, which propagates to `cold == true`, and the caller falls back to the server. In practice the
// backend already marks such flags requires_server_evaluation so they never reach this code — the COLD path
// here is the belt to that braces, and it fails SAFE.

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// coldConditionTypes are the DB-backed condition types: not resolvable in-process.
// Mirrors COLD_CONDITION_TYPES in pure.ts.
//
// Group PROPERTIES read the `groups` table. Group MEMBERSHIP ("group") does not — it comes from the
// caller-supplied groups map — so it stays purely evaluable.
var coldConditionTypes = map[string]struct{}{
	"segment":               {},
	"cohort":                {},
	"survey_answer":         {},
	"event_fired_in_window": {},
	"group_property":        {},
}

// IsColdConditionType reports whether a condition type can only be resolved by the database.
func IsColdConditionType(conditionType string) bool {
	_, ok := coldConditionTypes[conditionType]
	return ok
}

// AudienceCondition is one condition in the canonical targeting DSL.
type AudienceCondition struct {
	Type     string      `json:"type"`
	Operator string      `json:"operator"`
	Value    interface{} `json:"value,omitempty"`
	// Field is the attribute key, experiment id, or group property name, depending on Type.
	Field        string `json:"field,omitempty"`
	AttributeKey string `json:"attribute_key,omitempty"`
	// GroupType is the group dimension (e.g. "organisation") for group / group_property conditions.
	GroupType string `json:"group_type,omitempty"`
}

// AudienceConditions is the combinator + condition list of a rule.
type AudienceConditions struct {
	// Logic is "AND" (default) or "OR".
	Logic string              `json:"logic,omitempty"`
	Rules []AudienceCondition `json:"rules,omitempty"`
}

// AudienceRule is a flag's release conditions in the ONE canonical DSL — the shape the backend stores and
// serves verbatim.
type AudienceRule struct {
	Status string `json:"status,omitempty"`
	// Enabled is a pointer so that "absent" is distinguishable from an explicit false: only an explicit
	// false withholds the rule (mirrors `rule.enabled === false`).
	Enabled    *bool               `json:"enabled,omitempty"`
	StartDate  string              `json:"start_date,omitempty"`
	EndDate    string              `json:"end_date,omitempty"`
	SegmentIDs []string            `json:"segment_ids,omitempty"`
	Conditions *AudienceConditions `json:"conditions,omitempty"`
}

// AudienceContext is everything the evaluator may read about the caller.
//
// The scalar fields carry "" when unset, and an unset field is treated as ABSENT (JS `undefined`), not as
// the empty string — so e.g. `country not_any_of [US, CA]` does NOT match a context with no country, which
// is exactly what the backend does.
type AudienceContext struct {
	Country            string                 `json:"country,omitempty"`
	Platform           string                 `json:"platform,omitempty"`
	AppVersion         string                 `json:"app_version,omitempty"`
	SdkVersion         string                 `json:"sdk_version,omitempty"`
	CustomAttributes   map[string]interface{} `json:"custom_attributes,omitempty"`
	Groups             map[string]string      `json:"groups,omitempty"`
	ExperimentVariants map[string]string      `json:"experiment_variants,omitempty"`
}

// condResult is a tri-state: Go has no symbol sentinel, so COLD is a third value rather than a bool.
type condResult int

const (
	condFalse condResult = iota
	condTrue
	condCold
)

func condOf(b bool) condResult {
	if b {
		return condTrue
	}
	return condFalse
}

// EvaluateTargetingRule evaluates an audience rule against a context.
//
// matched is meaningful only when cold is false. cold==true means a DB-backed condition is load-bearing:
// the caller MUST re-resolve via the server and must never collapse it into false. Note that an OR rule
// already satisfied by a context condition is decided WITHOUT the server, even if it also contains a
// DB-backed condition.
func EvaluateTargetingRule(rule AudienceRule, ctx AudienceContext) (matched bool, cold bool) {
	if (rule.Enabled != nil && !*rule.Enabled) || rule.Status == "paused" || rule.Status == "archived" {
		return false, false
	}
	now := time.Now()
	if start, ok := tryParseDate(rule.StartDate); ok && start.After(now) {
		return false, false
	}
	if end, ok := tryParseDate(rule.EndDate); ok && end.Before(now) {
		return false, false
	}

	// Legacy `segment_ids` — fail CLOSED. These are a DB-backed allow-list we cannot read, and treating
	// them as absent would hand the flag to everyone.
	if len(rule.SegmentIDs) > 0 {
		return false, false
	}

	var conditions []AudienceCondition
	logic := "AND"
	if rule.Conditions != nil {
		conditions = rule.Conditions.Rules
		if rule.Conditions.Logic == "OR" {
			logic = "OR"
		}
	}
	if len(conditions) == 0 {
		return true, false
	}

	sawCold := false
	sawPureTrue := false
	sawPureFalse := false
	for _, condition := range conditions {
		switch evaluateCondition(condition, ctx) {
		case condCold:
			sawCold = true
		case condTrue:
			sawPureTrue = true
		default:
			sawPureFalse = true
		}
	}

	if logic == "OR" {
		if sawPureTrue {
			return true, false // determined
		}
		return false, sawCold
	}
	// AND
	if sawPureFalse {
		return false, false // determined
	}
	if sawCold {
		return false, true
	}
	return true, false
}

func evaluateCondition(condition AudienceCondition, ctx AudienceContext) condResult {
	if IsColdConditionType(condition.Type) {
		return condCold
	}

	switch condition.Type {
	case "group":
		// Group MEMBERSHIP is resolvable from the caller-supplied groups map alone.
		groupType := condition.GroupType
		if groupType == "" {
			groupType = condition.Field
		}
		if groupType == "" {
			return condFalse
		}
		return condOf(Compare(condition.Operator, lookupString(ctx.Groups, groupType), condition.Value))
	case "experiment_variant":
		experimentID := firstNonEmpty(condition.Field, condition.AttributeKey)
		if experimentID == "" || ctx.ExperimentVariants == nil {
			return condFalse
		}
		return condOf(Compare(condition.Operator, lookupString(ctx.ExperimentVariants, experimentID), condition.Value))
	case "attribute", "custom_attribute":
		key := firstNonEmpty(condition.Field, condition.AttributeKey)
		if key == "" {
			return condFalse
		}
		return condOf(Compare(condition.Operator, ctx.CustomAttributes[key], condition.Value))
	case "country":
		return condOf(Compare(condition.Operator, orNil(ctx.Country), condition.Value))
	case "platform":
		return condOf(Compare(condition.Operator, orNil(ctx.Platform), condition.Value))
	case "app_version":
		return condOf(Compare(condition.Operator, orNil(ctx.AppVersion), condition.Value))
	case "sdk_version":
		return condOf(Compare(condition.Operator, orNil(ctx.SdkVersion), condition.Value))
	case "subscription_status":
		return condOf(Compare(condition.Operator, ctx.CustomAttributes["subscription_status"], condition.Value))
	case "entitlement":
		return condOf(Compare(condition.Operator, ctx.CustomAttributes["entitlement_id"], condition.Value))
	}

	// Derived/unknown types yield false, matching the backend.
	return condFalse
}

// lookupString returns the map value as an interface, or nil when the key is ABSENT — never "". An absent
// value must read as JS `undefined` (so `is_not` / `not_any_of` / `not_contains` do NOT match), whereas a
// present empty string reads as present-but-empty.
func lookupString(m map[string]string, key string) interface{} {
	v, ok := m[key]
	if !ok {
		return nil
	}
	return v
}

// orNil maps an unset ("") context scalar onto absence. See AudienceContext.
func orNil(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Compare is the ONE operator matcher in this SDK. It mirrors the backend audience engine's compare() in
// supabase/functions/_shared/edge-serving/pure.ts byte-for-byte:
//   - exists / not_exists operate on presence + non-empty string.
//   - all other operators short-circuit to false when actual is absent.
//   - is/equals, is_not/not_equals, any_of/not_any_of, contains/not_contains compare on String(actual).
//   - gt/lt/gte/lte (+ their long forms) use date-or-number comparison.
//
// The gt/lt aliases are kept for back-compat with existing Go flag definitions alongside the backend's
// greater_than/less_than long forms.
func Compare(operator string, actual, expected interface{}) bool {
	actualString, actualPresent := toComparable(actual)

	if operator == "exists" {
		return actualPresent && len(actualString) > 0
	}
	if operator == "not_exists" {
		return !actualPresent || len(actualString) == 0
	}
	if !actualPresent {
		return false
	}

	switch operator {
	case "is", "equals":
		return actualString == toExpectedString(expected)
	case "is_not", "not_equals":
		return actualString != toExpectedString(expected)
	case "any_of":
		return expectedSliceContains(expected, actualString)
	case "not_any_of":
		ok, contained := expectedSliceContainsOK(expected, actualString)
		return ok && !contained
	case "contains":
		return strings.Contains(actualString, toExpectedString(expected))
	case "not_contains":
		return !strings.Contains(actualString, toExpectedString(expected))
	case "greater_than", "gt":
		return compareDateOrNumber(actual, actualString, expected, func(c int) bool { return c > 0 })
	case "less_than", "lt":
		return compareDateOrNumber(actual, actualString, expected, func(c int) bool { return c < 0 })
	case "greater_than_or_equal", "gte":
		return compareDateOrNumber(actual, actualString, expected, func(c int) bool { return c >= 0 })
	case "less_than_or_equal", "lte":
		return compareDateOrNumber(actual, actualString, expected, func(c int) bool { return c <= 0 })
	default:
		return false
	}
}

// toComparable mirrors pure.ts toComparable: nil -> absent; everything else -> its String() form.
// JSON-decoded float64 integers are rendered without a trailing ".0" so they match the
// String(actual) === String(expected) contract.
func toComparable(value interface{}) (string, bool) {
	if value == nil {
		return "", false
	}
	return stringify(value), true
}

// toExpectedString renders an expected scalar the way JS String(expected) would.
func toExpectedString(expected interface{}) string {
	if expected == nil {
		return "null"
	}
	return stringify(expected)
}

func stringify(v interface{}) string {
	switch n := v.(type) {
	case string:
		return n
	case bool:
		if n {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(n), 'f', -1, 64)
	case int, int8, int16, int32, int64, uint, uint32, uint64:
		f, _ := toNumber(v)
		return strconv.FormatFloat(f, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// expectedSliceContains reports whether expected is an array whose String()'d members include
// actualString (mirrors any_of).
func expectedSliceContains(expected interface{}, actualString string) bool {
	ok, contained := expectedSliceContainsOK(expected, actualString)
	return ok && contained
}

// expectedSliceContainsOK returns (expectedIsArray, contains). When expected is not an array,
// any_of/not_any_of both yield false (the Array.isArray(expected) gate).
func expectedSliceContainsOK(expected interface{}, actualString string) (bool, bool) {
	rv := reflect.ValueOf(expected)
	if expected == nil || (rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array) {
		return false, false
	}
	for i := 0; i < rv.Len(); i++ {
		if stringify(rv.Index(i).Interface()) == actualString {
			return true, true
		}
	}
	return true, false
}

// compareDateOrNumber mirrors pure.ts: if BOTH actual and expected parse as ISO dates, compare epoch
// millis; otherwise fall back to numeric comparison on Number(actualString) vs Number(expected).
// pass receives the sign of (a - b).
func compareDateOrNumber(actual interface{}, actualString string, expected interface{}, pass func(int) bool) bool {
	if ad, aok := tryParseDate(actual); aok {
		if ed, eok := tryParseDate(expected); eok {
			// Compare via After/Before rather than casting the nanosecond duration to int: time.Duration is
			// int64, and int is 32-bit on 32-bit targets (wasm, 32-bit ARM) — a >~2.1s gap would overflow
			// and flip the comparison. Pass a stable sign (-1/0/1).
			switch {
			case ad.After(ed):
				return pass(1)
			case ad.Before(ed):
				return pass(-1)
			default:
				return pass(0)
			}
		}
	}
	a, aok := numberFromString(actualString)
	b, bok := numberFromExpected(expected)
	if !aok || !bok {
		return false
	}
	switch {
	case a > b:
		return pass(1)
	case a < b:
		return pass(-1)
	default:
		return pass(0)
	}
}

// dateShape matches pure.ts /^\d{4}-\d{2}-\d{2}/ — guards against treating a plain number like "10" as a
// date.
var dateShape = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)

// tryParseDate mirrors pure.ts tryParseDate: only strings beginning with the YYYY-MM-DD shape AND parsing
// cleanly count as dates. RFC3339 first, then a bare date-only form.
func tryParseDate(v interface{}) (time.Time, bool) {
	s, ok := v.(string)
	if !ok || !dateShape.MatchString(s) {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// numberFromString mirrors JS `Number(string)`, which is NOT strconv.ParseFloat:
//
//   - Number("") === 0, and so does any whitespace-only string. This is load-bearing:
//     `score < 5` MATCHES a user whose score is "" on the backend, and the fixture pins it
//     (numeric-lt x score-empty). Returning "not a number" here would silently withhold the flag.
//   - Number("abc") is NaN, and every comparison against NaN is false — reported as ok=false.
//   - Go's ParseFloat additionally accepts "NaN"/"inf"/"INF", which JS does not (only the exact literal
//     "Infinity"). Those are rejected so a junk string can never compare equal.
func numberFromString(s string) (float64, bool) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, true
	}
	f, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || math.IsNaN(f) {
		return 0, false
	}
	if math.IsInf(f, 0) {
		switch trimmed {
		case "Infinity", "+Infinity", "-Infinity":
		default:
			return 0, false
		}
	}
	return f, true
}

func numberFromExpected(expected interface{}) (float64, bool) {
	if f, ok := toNumber(expected); ok {
		return f, true
	}
	if s, ok := expected.(string); ok {
		return numberFromString(s)
	}
	if b, ok := expected.(bool); ok {
		// JS Number(true) === 1, Number(false) === 0.
		if b {
			return 1, true
		}
		return 0, true
	}
	// nil expected: JS Number(undefined) is NaN — every comparison false.
	return 0, false
}

func toNumber(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}
