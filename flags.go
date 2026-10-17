package billdogeng

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// maxReportedFlagCalls bounds the (distinctId, flag, value) dedupe cache. A server
// SDK sees many users, so the cache is per-triple, not per-flag; when it fills, the
// OLDEST inserted entry is evicted (FIFO) so exposures for churned users are
// eventually re-emitted rather than the SDK leaking memory unboundedly.
const maxReportedFlagCalls = 50_000

// Flags is the feature-flag client supporting both remote and local (server-side)
// evaluation.
//
// Local evaluation (LocalEvaluation: true) fetches flag DEFINITIONS once, caches
// them with a 5-minute TTL, and evaluates each flag deterministically on this
// process — the cross-platform-identical algorithm shared with web/iOS/Android.
//
// Remote evaluation falls back to POST /experiment-config, which returns a
// pre-evaluated feature_flags map for the given user.
type Flags struct {
	transport       *transport
	apiKey          string
	localEvaluation bool
	enableLogging   bool

	// sendFeatureFlagEvents is the global toggle for `$feature_flag_called`
	// exposure events; capture routes them into the analytics pipeline.
	sendFeatureFlagEvents bool
	capture               func(distinctID, event string, properties Properties)

	// disabled makes every flag read inert (return nil/empty), with no network.
	disabled bool
	// pollingInterval is the TTL for cached local flag definitions (default 5m).
	pollingInterval time.Duration

	mu                 sync.Mutex
	definitions        map[string]FeatureFlagDefinition
	definitionsFetched time.Time
	loadOnce           sync.Mutex

	// reportedMu guards the exposure dedupe set. Go is concurrent — flag reads
	// happen on many goroutines, so the (distinctId, flag, value) dedupe state
	// MUST be protected. reportedOrder tracks insertion order for FIFO eviction.
	reportedMu    sync.Mutex
	reportedSet   map[string]struct{}
	reportedOrder []string
}

// flagsConfig groups the (growing) constructor arguments for newFlags.
type flagsConfig struct {
	localEvaluation       bool
	enableLogging         bool
	sendFeatureFlagEvents bool
	pollingInterval       time.Duration
	disabled              bool
	capture               func(distinctID, event string, properties Properties)
}

func newFlags(t *transport, apiKey string, cfg flagsConfig) *Flags {
	pollingInterval := cfg.pollingInterval
	if pollingInterval <= 0 {
		pollingInterval = 5 * time.Minute
	}
	return &Flags{
		transport:             t,
		apiKey:                apiKey,
		localEvaluation:       cfg.localEvaluation,
		enableLogging:         cfg.enableLogging,
		sendFeatureFlagEvents: cfg.sendFeatureFlagEvents,
		capture:               cfg.capture,
		disabled:              cfg.disabled,
		pollingInterval:       pollingInterval,
		definitions:           map[string]FeatureFlagDefinition{},
		reportedSet:           map[string]struct{}{},
	}
}

func (f *Flags) logf(format string, args ...interface{}) {
	if f.enableLogging {
		log.Printf("[BilldogEng:flags] "+format, args...)
	}
}

// GetFeatureFlag returns a flag's value for a user: a bool for a simple flag,
// the variant key string for a multivariate flag, or nil when the flag is unknown.
// serverOnly reports whether a flag's conditions need the database, so it cannot be evaluated locally.
// groupsOf pulls the caller's group map out of the eval options (nil-safe).
func groupsOf(opts *FlagEvalOptions) map[string]string {
	if opts == nil {
		return nil
	}
	return opts.Groups
}

func (f *Flags) serverOnly(key string) bool {
	f.mu.Lock()
	def, ok := f.definitions[key]
	f.mu.Unlock()
	return ok && def.RequiresServerEvaluation
}

// toAudienceContext builds the context the canonical evaluator reads, from the SDK's flag-eval options.
// An explicit context field wins; otherwise it falls back to a same-named person property, since most
// callers already carry country/platform in their property bag.
func toAudienceContext(opts *FlagEvalOptions) AudienceContext {
	if opts == nil {
		return AudienceContext{}
	}
	props := opts.PersonProperties
	return AudienceContext{
		Country:            firstNonEmpty(opts.Country, propString(props, "country")),
		Platform:           firstNonEmpty(opts.Platform, propString(props, "platform")),
		AppVersion:         firstNonEmpty(opts.AppVersion, propString(props, "app_version")),
		SdkVersion:         firstNonEmpty(opts.SdkVersion, propString(props, "sdk_version")),
		CustomAttributes:   props,
		Groups:             opts.Groups,
		ExperimentVariants: opts.ExperimentVariants,
	}
}

func propString(props Properties, key string) string {
	if s, ok := props[key].(string); ok {
		return s
	}
	return ""
}

func (f *Flags) GetFeatureFlag(ctx context.Context, key, distinctID string, opts *FlagEvalOptions) (FlagValue, error) {
	if f.disabled {
		return nil, nil
	}
	var personProps Properties
	if opts != nil {
		personProps = opts.PersonProperties
	}
	if f.localEvaluation {
		if err := f.ensureDefinitions(ctx); err != nil {
			return nil, err
		}
		f.mu.Lock()
		_, exists := f.definitions[key]
		f.mu.Unlock()
		if !exists {
			return nil, nil
		}
		// A flag whose conditions need the DB cannot be evaluated here — defer to the server rather than
		// ignore its conditions and wrongly turn a targeted flag ON.
		if !f.serverOnly(key) {
			verdict, cold := f.EvaluateLocally(key, distinctID, opts)
			if !cold {
				f.reportFeatureFlagCalled(key, distinctID, verdict, opts)
				return verdict, nil
			}
			// Defensive: the server said this flag was locally evaluable but a DB-backed condition turned
			// out to be load-bearing. Ask the server rather than guess.
		}
	}
	// onlyEvaluateLocally: caller forbids a server round-trip — return nil rather than ask.
	if opts != nil && opts.OnlyEvaluateLocally {
		return nil, nil
	}
	m, err := f.fetchRemote(ctx, distinctID, personProps, groupsOf(opts))
	if err != nil {
		return nil, err
	}
	v, ok := m[key]
	if !ok {
		return nil, nil
	}
	f.reportFeatureFlagCalled(key, distinctID, v, opts)
	return v, nil
}

// IsFeatureEnabled returns the boolean view of GetFeatureFlag (a variant string
// counts as ON).
func (f *Flags) IsFeatureEnabled(ctx context.Context, key, distinctID string, opts *FlagEvalOptions) (bool, error) {
	v, err := f.GetFeatureFlag(ctx, key, distinctID, opts)
	if err != nil {
		return false, err
	}
	if v == nil {
		return false, nil
	}
	if b, ok := v.(bool); ok {
		return b, nil
	}
	return true, nil
}

// GetFeatureFlagPayload returns a flag's payload (variant config). Only
// meaningful under local evaluation. Returns the matched variant's payload, else
// the flag-level payload, else nil.
func (f *Flags) GetFeatureFlagPayload(ctx context.Context, key, distinctID string, opts *FlagEvalOptions) (interface{}, error) {
	if f.disabled {
		return nil, nil
	}
	var personProps Properties
	onlyLocal := false
	if opts != nil {
		personProps = opts.PersonProperties
		onlyLocal = opts.OnlyEvaluateLocally
	}
	if err := f.ensureDefinitions(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	def, ok := f.definitions[key]
	f.mu.Unlock()
	if !ok {
		return nil, nil
	}
	var verdict FlagValue
	if f.serverOnly(key) {
		if onlyLocal {
			return nil, nil // cannot resolve without the server
		}
		m, err := f.fetchRemote(ctx, distinctID, personProps, groupsOf(opts))
		if err != nil {
			return nil, err
		}
		verdict = m[key]
		if verdict == nil {
			verdict = false
		}
	} else {
		local, cold := f.EvaluateLocally(key, distinctID, opts)
		if cold && onlyLocal {
			return nil, nil // a DB-backed condition went COLD and the caller forbids the server
		}
		if cold {
			// COLD conditions: resolve the verdict remotely rather than ignoring them.
			m, err := f.fetchRemote(ctx, distinctID, personProps, groupsOf(opts))
			if err != nil {
				return nil, err
			}
			verdict = m[key]
			if verdict == nil {
				// Parity with node's `?? false`: an absent remote verdict is OFF, which is
				// still an exposure (the user was resolved to "not enabled").
				verdict = false
			}
		} else {
			verdict = local
		}
	}
	f.reportFeatureFlagCalled(key, distinctID, verdict, opts)
	if b, isBool := verdict.(bool); isBool && !b {
		return nil, nil
	}
	if variantKey, isStr := verdict.(string); isStr {
		for _, v := range def.Variants {
			if v.Key == variantKey && v.Payload != nil {
				return v.Payload, nil
			}
		}
	}
	return def.Payload, nil
}

// GetAllFlags evaluates every known flag for a user.
func (f *Flags) GetAllFlags(ctx context.Context, distinctID string, opts *FlagEvalOptions) (map[string]FlagValue, error) {
	if f.disabled {
		return map[string]FlagValue{}, nil
	}
	var personProps Properties
	onlyLocal := false
	if opts != nil {
		personProps = opts.PersonProperties
		onlyLocal = opts.OnlyEvaluateLocally
	}
	if f.localEvaluation {
		if err := f.ensureDefinitions(ctx); err != nil {
			return nil, err
		}
		out := map[string]FlagValue{}
		f.mu.Lock()
		keys := make([]string, 0, len(f.definitions))
		for k := range f.definitions {
			keys = append(keys, k)
		}
		f.mu.Unlock()

		// Evaluate locally FIRST, so we learn which flags actually need the server — including any that go
		// COLD at runtime — and can then fetch them all in ONE request rather than one per flag. Never
		// approximate a server-only flag locally.
		local := make(map[string]FlagValue, len(keys))
		needServer := false
		for _, k := range keys {
			if f.serverOnly(k) {
				needServer = true
				continue
			}
			verdict, cold := f.EvaluateLocally(k, distinctID, opts)
			if cold {
				needServer = true
				continue
			}
			local[k] = verdict
		}
		// onlyEvaluateLocally: skip the server round-trip; flags that needed it resolve to false.
		var remote map[string]FlagValue
		if needServer && !onlyLocal {
			var err error
			remote, err = f.fetchRemote(ctx, distinctID, personProps, groupsOf(opts))
			if err != nil {
				return nil, err
			}
		}
		for _, k := range keys {
			if verdict, ok := local[k]; ok {
				out[k] = verdict
			} else if verdict, ok := remote[k]; ok {
				out[k] = verdict
			} else {
				out[k] = false
			}
		}
		return out, nil
	}
	// Non-local mode with onlyEvaluateLocally: no local definitions to evaluate from.
	if onlyLocal {
		return map[string]FlagValue{}, nil
	}
	m, err := f.fetchRemote(ctx, distinctID, personProps, groupsOf(opts))
	if err != nil {
		return nil, err
	}
	out := map[string]FlagValue{}
	for k, v := range m {
		out[k] = v
	}
	return out, nil
}

// GetAllFlagsAndPayloads evaluates every known flag AND its payload for a user.
// Payloads resolve in LOCAL mode (from cached definitions); in remote mode, and
// whenever a value is false, payload is nil. The value side matches GetAllFlags.
func (f *Flags) GetAllFlagsAndPayloads(ctx context.Context, distinctID string, opts *FlagEvalOptions) (map[string]FlagAndPayload, error) {
	if f.disabled {
		return map[string]FlagAndPayload{}, nil
	}
	values, err := f.GetAllFlags(ctx, distinctID, opts)
	if err != nil {
		return nil, err
	}
	out := make(map[string]FlagAndPayload, len(values))
	for k, v := range values {
		out[k] = FlagAndPayload{Value: v, Payload: f.payloadFor(k, v)}
	}
	return out, nil
}

// payloadFor is the payload a resolved verdict maps to, from cached definitions
// (matched variant payload → flag-level payload → nil). A false value → nil.
func (f *Flags) payloadFor(key string, value FlagValue) interface{} {
	if b, ok := value.(bool); ok && !b {
		return nil
	}
	f.mu.Lock()
	def, ok := f.definitions[key]
	f.mu.Unlock()
	if !ok {
		return nil
	}
	if variantKey, ok := value.(string); ok {
		for _, v := range def.Variants {
			if v.Key == variantKey && v.Payload != nil {
				return v.Payload
			}
		}
	}
	return def.Payload
}

// evaluateAllLocalSync synchronously resolves every locally-evaluable flag —
// used by Capture(..., CaptureOptions{SendFeatureFlags:true}) to attach
// `$feature/<key>` properties without blocking on a fetch. Server-only and COLD
// flags are skipped (they cannot be resolved here). Returns only flags whose
// verdict is determined locally.
func (f *Flags) evaluateAllLocalSync(distinctID string, groups map[string]string) map[string]FlagValue {
	out := map[string]FlagValue{}
	if f.disabled {
		return out
	}
	f.mu.Lock()
	keys := make([]string, 0, len(f.definitions))
	for k := range f.definitions {
		keys = append(keys, k)
	}
	f.mu.Unlock()
	opts := &FlagEvalOptions{Groups: groups}
	for _, k := range keys {
		if f.serverOnly(k) {
			continue
		}
		verdict, cold := f.EvaluateLocally(k, distinctID, opts)
		if !cold {
			out[k] = verdict
		}
	}
	return out
}

// ReloadFeatureFlagDefinitions forces a reload of the cached flag definitions
// (local mode).
func (f *Flags) ReloadFeatureFlagDefinitions(ctx context.Context) error {
	return f.fetchDefinitions(ctx)
}

// SetDefinitions injects flag definitions directly, bypassing the network.
// Primarily for tests and for hosts that distribute definitions out-of-band.
func (f *Flags) SetDefinitions(defs []FeatureFlagDefinition) {
	m := make(map[string]FeatureFlagDefinition, len(defs))
	for _, d := range defs {
		m[d.Key] = d
	}
	f.mu.Lock()
	f.definitions = m
	f.definitionsFetched = time.Now()
	f.mu.Unlock()
}

func (f *Flags) ensureDefinitions(ctx context.Context) error {
	f.mu.Lock()
	fresh := len(f.definitions) > 0 && time.Since(f.definitionsFetched) < f.pollingInterval
	f.mu.Unlock()
	if fresh {
		return nil
	}
	f.loadOnce.Lock()
	defer f.loadOnce.Unlock()
	// Re-check after acquiring the load lock (another goroutine may have loaded).
	f.mu.Lock()
	fresh = len(f.definitions) > 0 && time.Since(f.definitionsFetched) < f.pollingInterval
	f.mu.Unlock()
	if fresh {
		return nil
	}
	return f.fetchDefinitions(ctx)
}

func (f *Flags) fetchDefinitions(ctx context.Context) error {
	var data struct {
		Flags []FeatureFlagDefinition `json:"flags"`
	}
	err := f.transport.request(ctx, requestOptions{
		path:    "/feature-flag-definitions",
		body:    map[string]interface{}{"api_key": f.apiKey},
		headers: map[string]string{"X-BillDog-API-Key": f.apiKey},
		noGzip:  true,
	}, &data)
	if err != nil {
		// Graceful: keep any existing cache; surface in logs only.
		f.logf("failed to load flag definitions: %v", err)
		return nil
	}
	m := make(map[string]FeatureFlagDefinition, len(data.Flags))
	for _, d := range data.Flags {
		m[d.Key] = d
	}
	f.mu.Lock()
	f.definitions = m
	f.definitionsFetched = time.Now()
	f.mu.Unlock()
	f.logf("loaded %d flag definitions", len(m))
	return nil
}

func (f *Flags) fetchRemote(ctx context.Context, distinctID string, attributes Properties, groups map[string]string) (map[string]FlagValue, error) {
	if attributes == nil {
		attributes = Properties{}
	}
	if groups == nil {
		groups = map[string]string{}
	}
	var data struct {
		FeatureFlags map[string]FlagValue `json:"feature_flags"`
	}
	err := f.transport.request(ctx, requestOptions{
		path: "/experiment-config",
		body: map[string]interface{}{
			"api_key":    f.apiKey,
			"user_id":    distinctID,
			"attributes": attributes,
			// GROUP TARGETING: this param existed on the flag API and was silently DROPPED (the server had nowhere to put it). It now feeds the audience engine's group / group_property conditions.
			"groups": groups,
		},
		headers: map[string]string{"X-BillDog-API-Key": f.apiKey},
		noGzip:  true,
	}, &data)
	if err != nil {
		return nil, err
	}
	if data.FeatureFlags == nil {
		return map[string]FlagValue{}, nil
	}
	return data.FeatureFlags, nil
}

// reportFeatureFlagCalled emits the `$feature_flag_called` exposure event (PostHog parity) — the record
// that lets analytics and experiments attribute outcomes to the flag a user was exposed to. Deduped per
// (distinctId, key, value) so a hot flag read in a request loop bills at most one exposure per user per
// value. Fire-and-forget: the value is queued via capture, never awaited, so a flag read is never slowed
// or failed by telemetry. A nil verdict (unknown flag) is NOT reported — there was no exposure.
func (f *Flags) reportFeatureFlagCalled(key, distinctID string, response FlagValue, opts *FlagEvalOptions) {
	if response == nil {
		return
	}
	// Per-call override wins; otherwise the client-wide toggle decides.
	enabled := f.sendFeatureFlagEvents
	if opts != nil && opts.SendFeatureFlagEvents != nil {
		enabled = *opts.SendFeatureFlagEvents
	}
	if !enabled || f.capture == nil {
		return
	}

	dedupeKey := distinctID + ":" + key + ":" + fmt.Sprint(response)
	f.reportedMu.Lock()
	if _, seen := f.reportedSet[dedupeKey]; seen {
		f.reportedMu.Unlock()
		return
	}
	if len(f.reportedSet) >= maxReportedFlagCalls {
		// Evict the oldest (insertion-ordered) entry to bound memory.
		oldest := f.reportedOrder[0]
		f.reportedOrder = f.reportedOrder[1:]
		delete(f.reportedSet, oldest)
	}
	f.reportedSet[dedupeKey] = struct{}{}
	f.reportedOrder = append(f.reportedOrder, dedupeKey)
	f.reportedMu.Unlock()

	f.capture(distinctID, "$feature_flag_called", Properties{
		"$feature_flag":          key,
		"$feature_flag_response": response,
	})
}

// EvaluateLocally is the deterministic local flag evaluation (spec §D):
//  1. Missing/inactive -> false.
//  2. The flag's release conditions (TargetingRule) must match, else false. Resolved by the canonical
//     audience evaluator in targeting.go — AND/OR, date windows, rule status, the full operator set and
//     group membership all resolve here, with no server round-trip.
//  3. bucket = murmurhash3("flag:{key}:{distinctId}") % 100; ON iff bucket < rollout.
//  4. Multivariate: walk variants by cumulative rollout against an INDEPENDENT hash,
//     murmurhash3("flag-variant:{key}:{distinctId}") % 100 — NOT the rollout bucket.
//
// Returns (false, false) off, (true, false) on, (variantKey, false) for a multivariate hit, or
// (nil, true) — COLD — when a DB-backed condition turned out to be load-bearing and only the server can
// decide. Go has no symbol sentinel, so COLD is the second return value: "we don't know" and "not in the
// audience" are different answers, and conflating them is how targeting breaks quietly.
func (f *Flags) EvaluateLocally(key, distinctID string, opts *FlagEvalOptions) (verdict FlagValue, cold bool) {
	f.mu.Lock()
	def, ok := f.definitions[key]
	f.mu.Unlock()
	if !ok || !def.Active {
		return false, false
	}

	if def.TargetingRule != nil {
		matched, isCold := EvaluateTargetingRule(*def.TargetingRule, toAudienceContext(opts))
		// A DB-backed condition is load-bearing. We cannot resolve it and MUST NOT guess — treating it as
		// "no match" would silently withhold the flag, and treating it as a match would silently hand a
		// targeted feature to everyone. Tell the caller to ask the server.
		if isCold {
			return nil, true
		}
		if !matched {
			return false, false
		}
	}

	// CANONICAL bucketing — must match the backend byte-for-byte.
	// Conformance vectors: tests/fixtures/flag-bucketing.json (generated FROM the backend).
	bucket := float64(Murmurhash3("flag:"+key+":"+distinctID) % 100)
	if bucket >= def.RolloutPercentage {
		return false, false
	}

	if len(def.Variants) > 0 {
		// INDEPENDENT hash — reusing the rollout bucket starves later variants (at 10% rollout split
		// 50/50, every enabled user has bucket < 10, which is < 50, so variant B was never assigned).
		variantBucket := float64(Murmurhash3("flag-variant:"+key+":"+distinctID) % 100)
		cumulative := 0.0
		for _, variant := range def.Variants {
			cumulative += variant.RolloutPercentage
			if variantBucket < cumulative {
				return variant.Key, false
			}
		}
		return true, false // weights sum < 100 -> on, no specific variant
	}

	return true, false
}
