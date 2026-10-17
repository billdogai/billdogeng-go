package billdogeng

import "time"

// Properties is an arbitrary JSON-serializable property bag.
type Properties = map[string]interface{}

// Groups maps a group type to a group key (e.g. {"company": "acme", "team": "core"}).
type Groups = map[string]string

// Options configures a Client. All fields have defaults; the zero value is fine
// to pass — defaults are applied in New.
type Options struct {
	// Host is the base URL for all API requests. Default "https://api.billdog.io/v1".
	Host string
	// FlushAt is the batch size that triggers an automatic flush. Default 20.
	FlushAt int
	// FlushIntervalMs is the background flush cadence in milliseconds. Default 10000.
	FlushIntervalMs int
	// MaxQueueSize is the maximum number of queued events before the oldest are
	// dropped. Default 1000.
	MaxQueueSize int
	// Gzip gzips request bodies when large enough to benefit. Default true.
	Gzip bool
	// LocalEvaluation enables local (server-side) feature-flag evaluation. Default false.
	LocalEvaluation bool
	// RequestTimeoutMs is the per-request timeout in milliseconds. Default 10000.
	RequestTimeoutMs int
	// MaxRetries is the number of retry attempts for 5xx / network errors. Default 3.
	MaxRetries int
	// EnableLogging emits verbose diagnostics to stderr. Default false.
	EnableLogging bool
	// PersonProfiles controls PostHog-parity person-profile processing via the
	// reserved "$process_person_profile" wire flag. One of:
	//   "identified_only" (default) — attach nothing; identity is derived from
	//                       whether a user id is set (historical behavior).
	//   "never"           — attach false; events stay anonymous even after Identify.
	//   "always"          — attach true; ingest promotes even a pre-identify anon
	//                       id to a person.
	//                       ⚠️ WARNING: "always" makes EVERY distinct device count
	//                       toward billable MAU.
	// An empty/unset value behaves as "identified_only". A caller-supplied
	// "$process_person_profile" in an event's properties always wins.
	PersonProfiles string

	// SendFeatureFlagEvents globally toggles `$feature_flag_called` exposure
	// events (PostHog parity). nil means the default, true — every non-null flag
	// read emits ONE exposure event (deduped per user+flag+value) through the
	// analytics pipeline. Set to a pointer-to-false to disable them process-wide.
	// A per-call override lives on FlagEvalOptions and wins when present.
	SendFeatureFlagEvents *bool
	// GeoipResolutionLevel controls server-side GeoIP enrichment via the
	// `x-geoip-resolution-level` header on the /ingest-events flush. One of
	// "full" | "country_region" | "country_only" | "none". When empty (default)
	// no header is sent and the backend applies the project default ("full").
	GeoipResolutionLevel string

	// Disabled is a kill switch. When true the SDK is INERT: Capture/Identify/
	// GroupIdentify/Alias/SetPersonProperties are no-ops, flag reads return
	// nil/empty, NO background flush timer starts and NO network happens. Scoped
	// to analytics + flags (PostHog parity); explicit Surveys/Messaging/Llm calls
	// are NOT gated. Default false.
	Disabled bool

	// BeforeSend, when non-nil, runs on every event just before it is queued, at
	// the single enqueue choke point — so it sees Capture/Identify/GroupIdentify/
	// Alias AND the `$feature_flag_called` exposure. Return the (possibly mutated)
	// event to send it, or nil to DROP it.
	BeforeSend func(WireEvent) *WireEvent

	// FeatureFlagsPollingInterval is the TTL for cached local flag definitions —
	// how stale a definition set may get before the next flag read refetches it.
	// Zero means the default, 5 minutes.
	FeatureFlagsPollingInterval time.Duration

	// gzipSet / flushAtSet etc. track explicit zero-value overrides so that, for
	// example, Gzip:false is honored rather than treated as "unset". Set via the
	// fluent helpers below or left false (meaning "use field as given").
	gzipSet bool
}

// WithGzip explicitly sets the Gzip option (so an explicit false is honored).
func (o Options) WithGzip(v bool) Options {
	o.Gzip = v
	o.gzipSet = true
	return o
}

// PersonPropertyOps describes person-property operations persisted to the profile
// store via SetPersonProperties (PostHog `$set_once` parity). Operations are built
// in a fixed order — Set, SetOnce, Add, Append, Unset — into the `operations` wire
// array, mapping to `$set` / `$set_once` / `$add` / `$append` / `$unset`.
type PersonPropertyOps struct {
	// Set overwrites each property with the given value ($set).
	Set map[string]interface{}
	// SetOnce writes each property only if currently absent ($set_once).
	SetOnce map[string]interface{}
	// Add numerically increments each property by the given amount ($add).
	Add map[string]float64
	// Append pushes each value onto a list-valued property ($append).
	Append map[string]interface{}
	// Unset deletes each named property ($unset).
	Unset []string
}

// IdentifyOptions carries the optional setOnce map accepted by IdentifyWithOptions.
type IdentifyOptions struct {
	// SetOnce, when non-empty, ALSO persists `$set_once` properties to the profile
	// store via SetPersonProperties in addition to emitting the $identify event.
	SetOnce map[string]interface{}
}

// CaptureOptions are the per-call options accepted by CaptureWithOptions.
type CaptureOptions struct {
	// Timestamp overrides the event timestamp (for backfills / historical import).
	// A zero value (time.Time{}) means "now". Normalized to epoch ms on the wire.
	Timestamp time.Time
	// SendFeatureFlags attaches the user's active feature flags to the event as
	// `$feature/<key>` properties (plus `$active_feature_flags`), enabling
	// flag-breakdown analytics. Best-effort and LOCAL-only: uses already-cached
	// definitions and never blocks the capture on a fetch; server-only/cold flags
	// are skipped. A no-op unless LocalEvaluation is enabled.
	SendFeatureFlags bool
}

// WireEvent is the on-wire shape of a single analytics event (one batch element).
type WireEvent struct {
	EventName string `json:"event_name"`
	// EventTimestamp is epoch milliseconds.
	EventTimestamp int64      `json:"event_timestamp"`
	Properties     Properties `json:"properties"`
	// UserID maps from distinctId.
	UserID string `json:"user_id"`
}

// FlagEvalOptions are accepted by the feature-flag read methods.
type FlagEvalOptions struct {
	// Groups holds group memberships, e.g. {"company": "acme"}. Feeds the audience engine's `group`
	// conditions ("roll this feature out to Acme"), which are now evaluated LOCALLY — the map is
	// caller-supplied, so no database is needed.
	Groups Groups
	// PersonProperties are the person attributes, evaluated against `attribute` / `entitlement` /
	// `subscription_status` conditions.
	PersonProperties Properties
	// Country/Platform/AppVersion/SdkVersion are the evaluation context, read by the `country` /
	// `platform` / `app_version` / `sdk_version` conditions. Each falls back to a same-named person
	// property when left empty.
	Country    string
	Platform   string
	AppVersion string
	SdkVersion string
	// ExperimentVariants are the caller's experiment assignments, read by `experiment_variant`
	// conditions (flag dependencies).
	ExperimentVariants map[string]string
	// SendFeatureFlagEvents is a per-call override of the global option: when
	// non-nil it decides whether THIS read emits a `$feature_flag_called`
	// exposure event, winning over the client-wide setting. nil defers to the
	// global option (default true).
	SendFeatureFlagEvents *bool
	// OnlyEvaluateLocally forbids a server round-trip: resolve the flag purely
	// from local definitions. If a DB-backed (segment/cohort/…) condition is
	// load-bearing, or the SDK is not in local-evaluation mode, return nil/empty
	// instead of asking the server. Use when a remote round-trip is unacceptable.
	OnlyEvaluateLocally bool
}

// FlagVariant is one multivariate variant of a feature flag.
type FlagVariant struct {
	Key               string      `json:"key"`
	RolloutPercentage float64     `json:"rollout_percentage"`
	Payload           interface{} `json:"payload,omitempty"`
}

// FeatureFlagDefinition is a flag definition returned by /feature-flag-definitions.
type FeatureFlagDefinition struct {
	Key               string        `json:"key"`
	Active            bool          `json:"active"`
	RolloutPercentage float64       `json:"rollout_percentage"`
	Variants          []FlagVariant `json:"variants,omitempty"`
	// TargetingRule is the flag's release conditions, in the ONE canonical audience DSL, evaluated by
	// EvaluateTargetingRule in targeting.go.
	//
	// This replaced a flat {attribute, operator, value} array, which was a second condition DSL living in
	// nine SDKs and could not express OR, date windows, rule status, or group membership — so all of those
	// were needlessly round-tripped to the server.
	TargetingRule *AudienceRule `json:"targeting_rule,omitempty"`
	Payload       interface{}   `json:"payload,omitempty"`
	// RequiresServerEvaluation is TRUE when this flag's release conditions CANNOT be resolved
	// in-process — they hit the database (segment / cohort / survey_answer / event_fired_in_window /
	// group_property). The SDK MUST NOT evaluate such a flag locally: doing so ignores the conditions
	// entirely and turns a TARGETED flag ON for users who do not match it. Fall back to the server instead.
	RequiresServerEvaluation bool `json:"requires_server_evaluation,omitempty"`
}

// FlagValue is the result of evaluating a flag: a bool, a variant key (string),
// or nil for an unknown flag.
type FlagValue = interface{}

// FlagAndPayload is one entry returned by GetAllFlagsAndPayloads: a flag's
// resolved value alongside its payload. Payload resolves from cached local
// definitions (matched variant payload → flag-level payload → nil); in remote
// mode, and whenever Value is bool false, Payload is nil.
type FlagAndPayload struct {
	Value   FlagValue   `json:"value"`
	Payload interface{} `json:"payload"`
}

// ─── Surveys ────────────────────────────────────────────────────────────────

// SurveyListItem is one entry returned by surveys.List.
type SurveyListItem struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description,omitempty"`
	Type        string  `json:"type,omitempty"`
	Status      string  `json:"status,omitempty"`
}

// SurveyAnswer mirrors the bdsurvey-submit answer shape.
type SurveyAnswer struct {
	QuestionID   string      `json:"question_id"`
	ChoiceID     string      `json:"choice_id,omitempty"`
	AnswerText   string      `json:"answer_text,omitempty"`
	AnswerNumber *float64    `json:"answer_number,omitempty"`
	AnswerJSON   interface{} `json:"answer_json,omitempty"`
}

// SurveyContext carries optional context for survey lifecycle calls.
type SurveyContext struct {
	RespondentID   string
	CustomerID     string
	AnonymousID    string
	SessionID      string
	Platform       string
	DeviceInfo     Properties
	DurationMs     *int
	Context        Properties
	CollectorID    string
	CollectorType  string
	IdempotencyKey string
}

// SurveyConfiguration is the full configuration returned by /bdsurvey-fetch.
// Stored as a generic map so all (including new question types) fields pass
// through transparently.
type SurveyConfiguration = map[string]interface{}

// ─── Messaging ──────────────────────────────────────────────────────────────

// MessagingTargeting mirrors the messaging-dispatch targeting object.
type MessagingTargeting struct {
	Type          string      `json:"type,omitempty"` // all|advanced|individual
	Conditions    interface{} `json:"conditions,omitempty"`
	SegmentIDs    []string    `json:"segmentIds,omitempty"`
	Tags          []string    `json:"tags,omitempty"`
	SubscriberIDs []string    `json:"subscriberIds,omitempty"`
}

// MessagingScheduling mirrors the messaging-dispatch scheduling object.
type MessagingScheduling struct {
	DeliveryType string `json:"deliveryType,omitempty"` // immediate|scheduled|intelligent
	ScheduledAt  string `json:"scheduledAt,omitempty"`
	WindowStart  string `json:"windowStart,omitempty"`
	WindowEnd    string `json:"windowEnd,omitempty"`
}

// DispatchParams are the arguments to Messaging.Dispatch.
type DispatchParams struct {
	// ProjectID is the project UUID (required by messaging-dispatch).
	ProjectID string
	// Channel is one of push|email|sms|in-app|live-activity.
	Channel string
	Content Properties
	// Targeting/Scheduling are optional.
	Targeting  *MessagingTargeting
	Scheduling *MessagingScheduling
	TemplateID string
	// AccessToken is a Supabase session Bearer JWT; messaging-dispatch
	// authenticates by project membership, not X-BillDog-API-Key.
	AccessToken string
}

// ─── LLM ────────────────────────────────────────────────────────────────────

// LlmTraceParams are the arguments to Llm.CaptureTrace.
type LlmTraceParams struct {
	TraceID          string
	SpanID           string
	ParentSpanID     string
	Model            string
	InputText        string
	OutputText       string
	PromptTokens     int
	CompletionTokens int
	DurationMs       int
	CostUsd          float64
	Properties       Properties
	Metadata         Properties
	// Timestamp is ISO-8601; defaults to now if empty.
	Timestamp string
}
