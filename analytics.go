package billdogeng

import (
	"errors"
	"context"
	"fmt"
	"log"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// analytics is the capture/identify/groupIdentify/alias client with batching.
//
// Events accumulate in an in-memory queue and are flushed as a single batched
// POST to /ingest-events when:
//   - the queue reaches FlushAt,
//   - the background timer fires every FlushInterval, or
//   - Flush() / Shutdown() is called.
//
// A failed flush re-queues its batch (front of the queue) so events are never
// lost on a single transient failure; the transport also retries with backoff
// before a flush is considered failed.
type analytics struct {
	transport *transport
	apiKey    string
	opts      *resolvedOptions

	mu    sync.Mutex
	queue []WireEvent

	// flushMu serializes flushes so re-queued batches don't interleave.
	flushMu sync.Mutex

	// resolverMu guards flagResolver — it is late-bound by the client after
	// construction, then read on the Capture hot path from arbitrary goroutines.
	resolverMu   sync.RWMutex
	flagResolver func(distinctID string, groups Groups) map[string]FlagValue

	// quotaMu guards the quota-limited suspension state. A 2xx carrying a
	// non-empty `quota_limited` means the batch was NOT stored, so the flush path
	// goes dark: quotaSuspended stops flushing AND turns enqueue into a no-op
	// (data the server has already refused cannot be rescued by buffering it, and
	// an unbounded queue in a long-running process is a leak), until
	// quotaSuspendCooldown has elapsed since quotaSince and one probe flush may
	// test the meter again.
	quotaMu        sync.Mutex
	quotaSuspended bool
	quotaSince     time.Time

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func newAnalytics(t *transport, apiKey string, opts *resolvedOptions) *analytics {
	a := &analytics{
		transport: t,
		apiKey:    apiKey,
		opts:      opts,
		stopCh:    make(chan struct{}),
	}
	// Disabled SDK: stay fully inert — no background timer, no network.
	if !opts.Disabled {
		a.startTimer()
	}
	return a
}

// setFlagResolver late-binds the resolver used by Capture's SendFeatureFlags
// option (avoids a construction-order cycle: analytics is built before flags).
func (a *analytics) setFlagResolver(fn func(distinctID string, groups Groups) map[string]FlagValue) {
	a.resolverMu.Lock()
	a.flagResolver = fn
	a.resolverMu.Unlock()
}

func (a *analytics) logf(format string, args ...interface{}) {
	if a.opts.EnableLogging {
		log.Printf("[BilldogEng:analytics] "+format, args...)
	}
}

// warnf logs UNCONDITIONALLY — unlike logf, EnableLogging does not gate it. It
// is reserved for the pipeline going dark (and coming back), which is the one
// thing a developer must hear about even with diagnostics switched off.
func (a *analytics) warnf(format string, args ...interface{}) {
	log.Printf("[BilldogEng:analytics] WARN "+format, args...)
}

// Capture queues an arbitrary event for a user.
func (a *analytics) Capture(distinctID, event string, properties Properties, groups Groups) {
	a.CaptureWithOptions(distinctID, event, properties, groups, CaptureOptions{})
}

// CaptureWithOptions queues an event with per-call options: a backfill Timestamp
// and/or SendFeatureFlags (attach the user's active flags as `$feature/<key>` and
// `$active_feature_flags`). SendFeatureFlags is best-effort/local-only via the
// late-bound resolver — it never blocks capture on a fetch; server-only/cold
// flags are skipped.
func (a *analytics) CaptureWithOptions(distinctID, event string, properties Properties, groups Groups, opts CaptureOptions) {
	if a.opts.Disabled {
		return
	}
	props := a.withGroups(properties, groups)
	if opts.SendFeatureFlags {
		a.resolverMu.RLock()
		resolver := a.flagResolver
		a.resolverMu.RUnlock()
		if resolver != nil {
			active := resolver(distinctID, groups)
			enabled := make([]string, 0, len(active))
			for key, value := range active {
				props["$feature/"+key] = value
				// value !== false: everything except the boolean false counts as active.
				if b, ok := value.(bool); !ok || b {
					enabled = append(enabled, key)
				}
			}
			sort.Strings(enabled) // deterministic order (Go map iteration is random).
			props["$active_feature_flags"] = enabled
		}
	}
	a.enqueueAt(distinctID, event, props, resolveTimestamp(opts.Timestamp))
}

// CaptureException emits a canonical `$exception` event (PostHog error-tracking
// parity) THROUGH the normal capture pipeline, so it batches/flushes like any other
// event. It parses the error's concrete type / message into the `$exception_list`
// shape and — since Go errors carry no stack — captures the CURRENT stack at the
// call site via runtime.Callers so downstream error-tracking can group and
// symbolicate it. A server SDK requires distinctID (there is no ambient user).
//
// Disabled SDK → no-op. A malformed/empty stack never panics out of here.
func (a *analytics) CaptureException(err error, distinctID string, properties Properties) {
	if a.opts.Disabled {
		return
	}
	exc := buildExceptionEntry(err)
	props := Properties{}
	for k, v := range properties {
		props[k] = v
	}
	props["$exception_list"] = []exceptionEntry{exc}
	props["$exception_type"] = exc.Type
	props["$exception_message"] = exc.Value
	props["$exception_level"] = "error"
	a.Capture(distinctID, "$exception", props, nil)
}

// One entry in a `$exception_list` — PostHog's canonical exception shape. JSON tags
// are the wire contract (parity with the node SDK's ExceptionEntry/StackFrame).
type exceptionEntry struct {
	Type       string              `json:"type"`
	Value      string              `json:"value"`
	Mechanism  exceptionMechanism  `json:"mechanism"`
	Stacktrace exceptionStacktrace `json:"stacktrace"`
}

type exceptionMechanism struct {
	Handled   bool `json:"handled"`
	Synthetic bool `json:"synthetic"`
}

type exceptionStacktrace struct {
	Type   string       `json:"type"`
	Frames []stackFrame `json:"frames"`
}

type stackFrame struct {
	Filename string `json:"filename"`
	Function string `json:"function"`
	Lineno   int    `json:"lineno"`
	InApp    bool   `json:"in_app"`
}

// sdkModulePrefix identifies this SDK's own frames (marked in_app=false).
const sdkModulePrefix = "github.com/billdogai/billdogeng-go"

// buildExceptionEntry constructs a `$exception_list` entry from a Go error. type is
// the concrete error type (`%T`, e.g. "*errors.errorString"); for a nil error we
// fall back to the language default "error" with its string form and no frames (the
// Go analog of a non-exception / stackless value).
func buildExceptionEntry(err error) exceptionEntry {
	mech := exceptionMechanism{Handled: true, Synthetic: false}
	if err == nil {
		return exceptionEntry{
			Type:       "error",
			Value:      fmt.Sprintf("%v", err), // "<nil>"
			Mechanism:  mech,
			Stacktrace: exceptionStacktrace{Type: "raw", Frames: []stackFrame{}},
		}
	}
	return exceptionEntry{
		Type:       fmt.Sprintf("%T", err),
		Value:      err.Error(),
		Mechanism:  mech,
		Stacktrace: exceptionStacktrace{Type: "raw", Frames: captureStack(3)},
	}
}

// captureStack snapshots the current call stack (best-effort) as frames ordered
// OLDEST-FIRST. `skip` frames (runtime.Callers + this SDK's capture machinery) are
// dropped from the top; any that slip through are still marked in_app=false.
func captureStack(skip int) []stackFrame {
	var pcs [64]uintptr
	n := runtime.Callers(skip, pcs[:])
	if n == 0 {
		return []stackFrame{}
	}
	frames := runtime.CallersFrames(pcs[:n])
	out := make([]stackFrame, 0, n)
	for {
		fr, more := frames.Next()
		if fr.PC != 0 || fr.Function != "" || fr.File != "" {
			out = append(out, stackFrame{
				Filename: fr.File,
				Function: fr.Function,
				Lineno:   fr.Line,
				InApp:    frameInApp(fr.Function, fr.File),
			})
		}
		if !more {
			break
		}
	}
	// runtime yields newest-first; the wire contract wants oldest-first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// goRoot is the compile-time GOROOT, used to flag stdlib frames as not-in-app.
var goRoot = runtime.GOROOT()

// frameInApp reports whether a frame belongs to the user's application (true) vs
// the Go runtime/stdlib, a vendored dependency, or this SDK itself (false).
func frameInApp(function, file string) bool {
	if function == "" && file == "" {
		return false
	}
	if strings.HasPrefix(function, "runtime.") || strings.HasPrefix(function, "runtime/") {
		return false
	}
	if strings.HasPrefix(function, sdkModulePrefix) {
		return false // this SDK's own frames
	}
	if goRoot != "" && strings.HasPrefix(file, goRoot) {
		return false // GOROOT / stdlib
	}
	if strings.Contains(file, "/vendor/") {
		return false
	}
	return true
}

// resolveTimestamp normalizes a caller-supplied timestamp to epoch ms; the zero
// time.Time means "now".
func resolveTimestamp(ts time.Time) int64 {
	if ts.IsZero() {
		return time.Now().UnixMilli()
	}
	return ts.UnixMilli()
}

// Identify queues an $identify event with $set.
func (a *analytics) Identify(distinctID string, properties Properties) {
	if properties == nil {
		properties = Properties{}
	}
	props := Properties{"$set": properties}
	for k, v := range properties {
		props[k] = v
	}
	a.enqueue(distinctID, "$identify", props)
}

// IdentifyWithOptions queues an $identify event and, when opts.SetOnce is non-empty,
// ALSO persists those `$set_once` properties to the profile store via
// SetPersonProperties. Default behaviour (empty SetOnce) is identical to Identify.
func (a *analytics) IdentifyWithOptions(distinctID string, properties Properties, opts IdentifyOptions) {
	a.Identify(distinctID, properties)
	if len(opts.SetOnce) > 0 {
		a.SetPersonProperties(distinctID, PersonPropertyOps{SetOnce: opts.SetOnce})
	}
}

// SetPersonProperties applies person-property operations to the profile store
// (POST /user-properties) — the PostHog-parity op vocabulary the $identify event
// alone cannot express: $set (overwrite), $set_once (write only if absent),
// $add (numeric increment), $append (push to a list), $unset (delete).
//
// Fire-and-forget: the request runs on a background goroutine and its (already
// retried) transport error is swallowed, so this never blocks or panics the
// caller. Empty ops issue no request. Shutdown drains any in-flight request.
func (a *analytics) SetPersonProperties(distinctID string, ops PersonPropertyOps) {
	if a.opts.Disabled {
		return
	}
	operations := make([]map[string]interface{}, 0)
	for k, v := range ops.Set {
		operations = append(operations, map[string]interface{}{"operation": "$set", "key": k, "value": v})
	}
	for k, v := range ops.SetOnce {
		operations = append(operations, map[string]interface{}{"operation": "$set_once", "key": k, "value": v})
	}
	for k, v := range ops.Add {
		operations = append(operations, map[string]interface{}{"operation": "$add", "key": k, "value": v})
	}
	for k, v := range ops.Append {
		operations = append(operations, map[string]interface{}{"operation": "$append", "key": k, "value": v})
	}
	for _, k := range ops.Unset {
		operations = append(operations, map[string]interface{}{"operation": "$unset", "key": k})
	}
	if len(operations) == 0 {
		return
	}

	body := map[string]interface{}{
		"api_key":     a.apiKey,
		"distinct_id": distinctID,
		"operations":  operations,
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		// Best-effort: the transport already retries transient failures; a final
		// error is swallowed so a property-persist never surfaces to the caller.
		_ = a.transport.request(context.Background(), requestOptions{
			path:    "/user-properties",
			body:    body,
			headers: map[string]string{"X-BillDog-API-Key": a.apiKey},
			noGzip:  true,
		}, nil)
	}()
}

// GroupIdentify queues a $groupidentify event.
func (a *analytics) GroupIdentify(groupType, groupKey string, properties Properties) {
	if properties == nil {
		properties = Properties{}
	}
	a.enqueue(groupKey, "$groupidentify", Properties{
		"$group_type": groupType,
		"$group_key":  groupKey,
		"$group_set":  properties,
	})
}

// Alias queues a $create_alias event.
func (a *analytics) Alias(distinctID, alias string) {
	a.enqueue(distinctID, "$create_alias", Properties{
		"alias":       alias,
		"distinct_id": distinctID,
	})
}

func (a *analytics) withGroups(properties Properties, groups Groups) Properties {
	out := Properties{}
	for k, v := range properties {
		out[k] = v
	}
	if len(groups) == 0 {
		return out
	}
	groupsCopy := Groups{}
	for k, v := range groups {
		groupsCopy[k] = v
	}
	out["$groups"] = groupsCopy
	return out
}

// applyPersonProfile resolves the PersonProfiles mode into the reserved
// "$process_person_profile" wire flag (PostHog parity). A caller-supplied value
// always wins; "never" attaches false, "always" attaches true, and
// "identified_only"/unset attaches nothing.
func (a *analytics) applyPersonProfile(properties Properties) Properties {
	if properties == nil {
		properties = Properties{}
	}
	if _, ok := properties["$process_person_profile"]; ok {
		return properties
	}
	switch a.opts.PersonProfiles {
	case "never":
		properties["$process_person_profile"] = false
	case "always":
		properties["$process_person_profile"] = true
	}
	return properties
}

func (a *analytics) enqueue(distinctID, eventName string, properties Properties) {
	a.enqueueAt(distinctID, eventName, properties, time.Now().UnixMilli())
}

func (a *analytics) enqueueAt(distinctID, eventName string, properties Properties, timestamp int64) {
	// Disabled SDK: no events, no network. Covers Identify/GroupIdentify/Alias too.
	if a.opts.Disabled {
		return
	}
	// Quota-limited: the server is discarding this project's data, so queueing
	// more of it would only grow a queue that can never drain. Cheap no-op until
	// the cooldown re-opens the flush path.
	if a.quotaSuspendedNow() {
		return
	}
	properties = a.applyPersonProfile(properties)
	wireEvent := WireEvent{
		EventName:      eventName,
		EventTimestamp: timestamp,
		Properties:     properties,
		UserID:         distinctID,
	}

	// beforeSend hook: mutate or DROP (nil) the event before it is queued. Runs at
	// this single choke point, so it sees capture/identify/group/alias AND the
	// `$feature_flag_called` exposure.
	if a.opts.BeforeSend != nil {
		out := a.opts.BeforeSend(wireEvent)
		if out == nil {
			return
		}
		wireEvent = *out
	}

	a.mu.Lock()
	a.queue = append(a.queue, wireEvent)
	if len(a.queue) > a.opts.MaxQueueSize {
		dropped := len(a.queue) - a.opts.MaxQueueSize
		a.queue = a.queue[dropped:]
		a.logf("max queue size exceeded, dropped %d oldest event(s)", dropped)
	}
	shouldFlush := len(a.queue) >= a.opts.FlushAt
	a.mu.Unlock()

	if shouldFlush {
		go func() { _ = a.Flush() }()
	}
}

// QueueLength returns the number of queued (not-yet-flushed) events.
func (a *analytics) QueueLength() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.queue)
}

// Flush force-flushes the queue immediately and returns once the in-flight
// batch settles.
func (a *analytics) Flush() error {
	a.flushMu.Lock()
	defer a.flushMu.Unlock()
	return a.doFlush()
}

func (a *analytics) doFlush() error {
	a.mu.Lock()
	if len(a.queue) == 0 {
		a.mu.Unlock()
		return nil
	}
	batch := a.queue
	a.queue = nil
	a.mu.Unlock()

	if !a.quotaAllowFlush() {
		// Still dark: whatever we send is discarded, so drop the batch instead of
		// holding it. These are the events queued just before the suspension —
		// enqueue is a no-op from here until the cooldown elapses.
		a.logf("quota limited, dropped %d event(s) without sending", len(batch))
		return newError(fmt.Sprintf("quota limited: flushing suspended, %d event(s) dropped", len(batch)), 0, quotaLimitedCode, nil)
	}

	body := map[string]interface{}{
		"api_key": a.apiKey,
		"events":  batch,
	}

	headers := map[string]string{"X-BillDog-API-Key": a.apiKey}
	// GeoIP resolution control: only attach the header when configured; an unset
	// value lets the backend apply its project default ("full"). This header
	// rides ONLY on the /ingest-events flush.
	if a.opts.GeoipResolutionLevel != "" {
		headers["x-geoip-resolution-level"] = a.opts.GeoipResolutionLevel
	}

	err := a.transport.request(context.Background(), requestOptions{
		path:    "/ingest-events",
		body:    body,
		headers: headers,
	}, nil)
	if err != nil {
		if be, ok := err.(*Error); ok && be.Code == quotaLimitedCode {
			// The server answered 2xx but stored nothing. Re-queueing a batch it has
			// already refused would grow the queue forever, so drop it and go dark.
			a.quotaSuspend(be.Message)
			return err
		}
		// Re-queue the batch at the front so it is retried on the next flush.
		a.mu.Lock()
		a.queue = append(batch, a.queue...)
		a.mu.Unlock()
		a.logf("flush failed, re-queued %d event(s): %v", len(batch), err)
		return err
	}
	a.quotaRecover()
	a.logf("flushed %d event(s)", len(batch))
	return nil
}

// quotaSuspendedNow reports whether the flush path is dark right now. It is the
// enqueue guard, so it never advances the cooldown; once the cooldown has
// elapsed events are accepted again to give the probe flush something to carry.
func (a *analytics) quotaSuspendedNow() bool {
	a.quotaMu.Lock()
	defer a.quotaMu.Unlock()
	return a.quotaSuspended && time.Since(a.quotaSince) < quotaSuspendCooldown
}

// quotaAllowFlush reports whether a flush may go out, CONSUMING the probe when
// the cooldown has elapsed: exactly one flush per cooldown re-tests the meter,
// so a process that outlives the allowance recovers on its own without hammering
// an endpoint that is discarding everything.
func (a *analytics) quotaAllowFlush() bool {
	a.quotaMu.Lock()
	defer a.quotaMu.Unlock()
	if !a.quotaSuspended {
		return true
	}
	if time.Since(a.quotaSince) < quotaSuspendCooldown {
		return false
	}
	a.quotaSince = time.Now() // probe consumed; quotaSuspend re-arms it if it fails
	return true
}

// quotaSuspend darkens the flush path after the server reported a drained meter.
// The reason is logged EXACTLY ONCE per suspension — it is the only signal a
// developer gets that their pipeline stopped recording — so a failed probe only
// re-arms the cooldown and stays quiet.
func (a *analytics) quotaSuspend(reason string) {
	a.quotaMu.Lock()
	first := !a.quotaSuspended
	a.quotaSuspended = true
	a.quotaSince = time.Now()
	a.quotaMu.Unlock()
	if first {
		a.warnf("quota limited: %s (no events will be sent for %s)", reason, quotaSuspendCooldown)
	}
}

// quotaRecover lifts the suspension once a probe flush actually stored its
// batch, and says so once — the counterpart to the warning, so a developer
// watching the log sees the pipeline come back rather than guessing.
func (a *analytics) quotaRecover() {
	a.quotaMu.Lock()
	was := a.quotaSuspended
	a.quotaSuspended = false
	a.quotaMu.Unlock()
	if was {
		a.warnf("quota allowance restored — events are being sent again")
	}
}

func (a *analytics) startTimer() {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ticker := time.NewTicker(time.Duration(a.opts.FlushIntervalMs) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = a.Flush()
			case <-a.stopCh:
				return
			}
		}
	}()
}

func (a *analytics) stopTimer() {
	a.stopOnce.Do(func() {
		close(a.stopCh)
	})
	a.wg.Wait()
}

// Shutdown flushes remaining events and stops the background flush timer.
func (a *analytics) Shutdown() error {
	a.stopTimer()
	err := a.Flush()
	// A quota refusal must never fail Shutdown. Shutdown is what callers put in
	// a defer, and the common shape is `defer func() { if err := c.Shutdown();
	// err != nil { log.Fatal(err) } }()` — so propagating it here would take a
	// customer's server down on the day their allowance ran out. That is the
	// precise failure the whole 200-instead-of-402 contract exists to prevent
	// (see supabase/functions/_shared/quota-limited.ts).
	//
	// Flush still reports it. An explicit Flush is a caller asking whether the
	// data landed, and it deserves the truth; a deferred Shutdown is not.
	if errors.Is(err, ErrQuotaLimited) {
		return nil
	}
	return err
}
