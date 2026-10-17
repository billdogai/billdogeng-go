// Package billdogeng is the official BilldogEng server SDK for Go.
//
// Engagement suite for server-side use: Analytics, Feature Flags (remote +
// local evaluation), Surveys (data API), Messaging dispatch, and LLM tracing.
//
// Quickstart:
//
//	bd, err := billdogeng.New("bd_test_xxx", billdogeng.Options{LocalEvaluation: true})
//	if err != nil { log.Fatal(err) }
//	bd.Capture("user-123", "order_completed", billdogeng.Properties{"revenue": 49.99}, nil)
//	on, _ := bd.IsFeatureEnabled(ctx, "new_checkout", "user-123", nil)
//	_ = bd.Shutdown()
package billdogeng

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// resolvedOptions is the fully-defaulted configuration.
type resolvedOptions struct {
	Host             string
	FlushAt          int
	FlushIntervalMs  int
	MaxQueueSize     int
	Gzip             bool
	LocalEvaluation  bool
	RequestTimeoutMs int
	MaxRetries       int
	EnableLogging    bool
	PersonProfiles   string
	// SendFeatureFlagEvents is the resolved global toggle for `$feature_flag_called`
	// exposure events. Defaults to true.
	SendFeatureFlagEvents bool
	// GeoipResolutionLevel is the resolved GeoIP header value; empty means omit.
	GeoipResolutionLevel string
	// Disabled is the resolved kill switch (analytics + flags inert). Default false.
	Disabled bool
	// BeforeSend is the resolved enqueue-time hook; nil means no hook.
	BeforeSend func(WireEvent) *WireEvent
	// FeatureFlagsPollingInterval is the resolved TTL for cached local flag
	// definitions. Defaults to 5 minutes.
	FeatureFlagsPollingInterval time.Duration
}

func resolveOptions(o Options) *resolvedOptions {
	r := &resolvedOptions{
		Host:             "https://api.billdog.io/v1",
		FlushAt:          20,
		FlushIntervalMs:  10000,
		MaxQueueSize:     1000,
		Gzip:             true,
		LocalEvaluation:  false,
		RequestTimeoutMs: 10000,
		MaxRetries:       3,
		EnableLogging:    false,
		PersonProfiles:   "identified_only",
		// Exposure events are ON by default (PostHog parity); only an explicit
		// pointer-to-false disables them.
		SendFeatureFlagEvents: true,
		// Local flag-definitions cache TTL (spec §D).
		FeatureFlagsPollingInterval: 5 * time.Minute,
	}
	if o.Host != "" {
		r.Host = o.Host
	}
	if o.FlushAt > 0 {
		r.FlushAt = o.FlushAt
	}
	if o.FlushIntervalMs > 0 {
		r.FlushIntervalMs = o.FlushIntervalMs
	}
	if o.MaxQueueSize > 0 {
		r.MaxQueueSize = o.MaxQueueSize
	}
	if o.gzipSet {
		r.Gzip = o.Gzip
	}
	r.LocalEvaluation = o.LocalEvaluation
	if o.RequestTimeoutMs > 0 {
		r.RequestTimeoutMs = o.RequestTimeoutMs
	}
	if o.MaxRetries > 0 {
		r.MaxRetries = o.MaxRetries
	}
	r.EnableLogging = o.EnableLogging
	if o.PersonProfiles != "" {
		r.PersonProfiles = o.PersonProfiles
	}
	if o.SendFeatureFlagEvents != nil {
		r.SendFeatureFlagEvents = *o.SendFeatureFlagEvents
	}
	r.GeoipResolutionLevel = o.GeoipResolutionLevel
	r.Disabled = o.Disabled
	r.BeforeSend = o.BeforeSend
	if o.FeatureFlagsPollingInterval > 0 {
		r.FeatureFlagsPollingInterval = o.FeatureFlagsPollingInterval
	}
	return r
}

// Client is the BilldogEng server SDK client.
type Client struct {
	options *resolvedOptions

	transport *transport
	analytics *analytics

	// Flags is the feature-flags client (remote + local evaluation).
	Flags *Flags
	// Surveys is the Surveys data API.
	Surveys *Surveys
	// Messaging is the messaging dispatch client.
	Messaging *Messaging
	// Llm is the LLM observability client.
	Llm *Llm
}

// New constructs a Client. apiKey is required (bd_test_* sandbox / bd_live_* live).
//
// An optional *http.Client may be supplied (primarily for tests) via the
// variadic httpClient parameter; the first non-nil one wins.
func New(apiKey string, opts Options, httpClient ...*http.Client) (*Client, error) {
	if apiKey == "" {
		return nil, errors.New("billdogeng: apiKey is required")
	}
	resolved := resolveOptions(opts)

	var hc *http.Client
	for _, c := range httpClient {
		if c != nil {
			hc = c
			break
		}
	}

	t := newTransport(transportConfig{
		host:           resolved.Host,
		requestTimeout: time.Duration(resolved.RequestTimeoutMs) * time.Millisecond,
		gzip:           resolved.Gzip,
		maxRetries:     resolved.MaxRetries,
		httpClient:     hc,
		enableLogging:  resolved.EnableLogging,
	})

	a := newAnalytics(t, apiKey, resolved)
	// Wire the Flags module's exposure capture into the analytics pipeline so
	// `$feature_flag_called` events are batched + billed like any other event.
	// Fire-and-forget: Capture only enqueues, so a flag read never blocks on the
	// network. Groups are nil — an exposure event carries no group context.
	capture := func(distinctID, event string, properties Properties) {
		a.Capture(distinctID, event, properties, nil)
	}
	f := newFlags(t, apiKey, flagsConfig{
		localEvaluation:       resolved.LocalEvaluation,
		enableLogging:         resolved.EnableLogging,
		sendFeatureFlagEvents: resolved.SendFeatureFlagEvents,
		pollingInterval:       resolved.FeatureFlagsPollingInterval,
		disabled:              resolved.Disabled,
		capture:               capture,
	})
	// Late-bind the flag resolver so Capture(..., CaptureOptions{SendFeatureFlags:true})
	// can inject the user's active flags without a construction-order cycle
	// (analytics is built before flags, but flags depends on analytics.capture).
	a.setFlagResolver(func(distinctID string, groups Groups) map[string]FlagValue {
		return f.evaluateAllLocalSync(distinctID, groups)
	})
	c := &Client{
		options:   resolved,
		transport: t,
		analytics: a,
		Flags:     f,
		Surveys:   newSurveys(t, apiKey),
		Messaging: newMessaging(t),
		Llm:       newLlm(t, apiKey),
	}
	return c, nil
}

// ─── Analytics ──────────────────────────────────────────────────────────────

// Capture queues an event for a user. Batched; flushed per the configured policy.
func (c *Client) Capture(distinctID, event string, properties Properties, groups Groups) {
	c.analytics.Capture(distinctID, event, properties, groups)
}

// CaptureWithOptions queues an event with per-call CaptureOptions: a backfill
// Timestamp and/or SendFeatureFlags (attach the user's active flags as
// `$feature/<key>` + `$active_feature_flags`, best-effort/local-only). Capture is
// CaptureWithOptions with the zero CaptureOptions.
func (c *Client) CaptureWithOptions(distinctID, event string, properties Properties, groups Groups, opts CaptureOptions) {
	c.analytics.CaptureWithOptions(distinctID, event, properties, groups, opts)
}

// CaptureException emits a canonical `$exception` event (PostHog error-tracking
// parity) for err, attributed to distinctID, merging any caller-supplied
// properties. Go errors carry no stack, so the CURRENT call-site stack is captured
// via runtime.Callers. Batched/flushed like any other event; no-op when disabled.
func (c *Client) CaptureException(err error, distinctID string, properties Properties) {
	c.analytics.CaptureException(err, distinctID, properties)
}

// Identify sets person properties. Emits $identify.
func (c *Client) Identify(distinctID string, properties Properties) {
	c.analytics.Identify(distinctID, properties)
}

// IdentifyWithOptions sets person properties (emits $identify) and, when
// opts.SetOnce is non-empty, ALSO persists `$set_once` properties to the profile
// store via SetPersonProperties. Identify is IdentifyWithOptions with empty opts.
func (c *Client) IdentifyWithOptions(distinctID string, properties Properties, opts IdentifyOptions) {
	c.analytics.IdentifyWithOptions(distinctID, properties, opts)
}

// SetPersonProperties persists person-property operations ($set / $set_once /
// $add / $append / $unset) to the profile store via POST /user-properties.
// Fire-and-forget; empty ops issue no request.
func (c *Client) SetPersonProperties(distinctID string, ops PersonPropertyOps) {
	c.analytics.SetPersonProperties(distinctID, ops)
}

// GroupIdentify sets group properties. Emits $groupidentify.
func (c *Client) GroupIdentify(groupType, groupKey string, properties Properties) {
	c.analytics.GroupIdentify(groupType, groupKey, properties)
}

// Alias aliases one distinct id to another. Emits $create_alias.
func (c *Client) Alias(distinctID, alias string) {
	c.analytics.Alias(distinctID, alias)
}

// Flush flushes queued analytics events now.
func (c *Client) Flush() error {
	return c.analytics.Flush()
}

// QueueLength returns the number of events currently queued (test/observability aid).
func (c *Client) QueueLength() int {
	return c.analytics.QueueLength()
}

// ─── Feature flags ──────────────────────────────────────────────────────────

// GetFeatureFlag returns a flag value: bool, variant key string, or nil if unknown.
func (c *Client) GetFeatureFlag(ctx context.Context, key, distinctID string, opts *FlagEvalOptions) (FlagValue, error) {
	return c.Flags.GetFeatureFlag(ctx, key, distinctID, opts)
}

// IsFeatureEnabled reports whether a flag is enabled for the user.
func (c *Client) IsFeatureEnabled(ctx context.Context, key, distinctID string, opts *FlagEvalOptions) (bool, error) {
	return c.Flags.IsFeatureEnabled(ctx, key, distinctID, opts)
}

// GetFeatureFlagPayload returns a flag's payload (variant config), local mode.
func (c *Client) GetFeatureFlagPayload(ctx context.Context, key, distinctID string, opts *FlagEvalOptions) (interface{}, error) {
	return c.Flags.GetFeatureFlagPayload(ctx, key, distinctID, opts)
}

// GetAllFlags evaluates all known flags for a user.
func (c *Client) GetAllFlags(ctx context.Context, distinctID string, opts *FlagEvalOptions) (map[string]FlagValue, error) {
	return c.Flags.GetAllFlags(ctx, distinctID, opts)
}

// GetAllFlagsAndPayloads evaluates all known flags AND their payloads for a user.
// Payloads resolve in local mode (from cached definitions); in remote mode, and
// whenever a value is false, payload is nil. The value side matches GetAllFlags.
func (c *Client) GetAllFlagsAndPayloads(ctx context.Context, distinctID string, opts *FlagEvalOptions) (map[string]FlagAndPayload, error) {
	return c.Flags.GetAllFlagsAndPayloads(ctx, distinctID, opts)
}

// ReloadFeatureFlagDefinitions reloads local flag definitions (local mode).
func (c *Client) ReloadFeatureFlagDefinitions(ctx context.Context) error {
	return c.Flags.ReloadFeatureFlagDefinitions(ctx)
}

// ─── Convenience ────────────────────────────────────────────────────────────

// DispatchMessage dispatches a message (see Messaging.Dispatch).
func (c *Client) DispatchMessage(ctx context.Context, params DispatchParams) (DispatchResult, error) {
	return c.Messaging.Dispatch(ctx, params)
}

// CaptureTrace captures an LLM trace span (see Llm.CaptureTrace).
func (c *Client) CaptureTrace(ctx context.Context, params LlmTraceParams, out interface{}) error {
	return c.Llm.CaptureTrace(ctx, params, out)
}

// ─── Lifecycle ──────────────────────────────────────────────────────────────

// Shutdown flushes remaining events and stops the background flush timer.
func (c *Client) Shutdown() error {
	return c.analytics.Shutdown()
}
