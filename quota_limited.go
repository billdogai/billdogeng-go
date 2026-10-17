package billdogeng

import (
	"errors"
	"strings"
	"time"
)

// Client half of the `quota_limited` contract.
//
// The ingestion endpoints answer an exhausted allowance with HTTP 200 and a
// top-level `quota_limited` array naming the drained meters, rather than an
// error status — so a customer's app never breaks because their BillDog meter
// is full. See supabase/functions/_shared/quota-limited.ts for that reasoning.
//
// The cost of that choice is that a 2xx no longer proves the data was stored. A
// client that reads every 200 as success keeps transmitting into a closed
// meter — every batch discarded, every flush reported as a success — so the
// transport surfaces such a body as an *Error carrying quotaLimitedCode, and
// the flush path goes dark until the meter can plausibly have reopened.

// quotaLimitedCode is the Error.Code carried by a 2xx that stored nothing.
const quotaLimitedCode = "QUOTA_LIMITED"

// quotaSuspendCooldown is how long the flush path stays dark before ONE probe
// flush is let through. The mobile SDKs suspend for the whole session, which is
// wrong for a long-running server process: an allowance resets monthly and a
// customer may upgrade at any moment, so staying dark for the life of the
// process is as wrong as never stopping at all.
const quotaSuspendCooldown = 15 * time.Minute

// ErrQuotaLimited matches, via errors.Is, the error a flush returns when the
// server answered 2xx but stored nothing because an allowance — the plan
// envelope, a sandbox cap, or the account's own spend limit — is exhausted.
// Those events are gone; nothing is queued for a later retry.
var ErrQuotaLimited = errors.New("billdogeng: quota limited")

// quotaLimitedMetrics names the drained meters reported by a parsed 2xx body,
// or nil when the body is not a quota refusal.
//
// Deliberately tolerant: the argument is a response body the SDK does not
// control, so anything unexpected — a bare string, a null, the wrong types —
// reads as "not limited" rather than failing inside the transport path.
func quotaLimitedMetrics(parsed interface{}) []string {
	body, ok := parsed.(map[string]interface{})
	if !ok {
		return nil
	}
	raw, ok := body["quota_limited"].([]interface{})
	if !ok {
		return nil
	}
	metrics := make([]string, 0, len(raw))
	for _, entry := range raw {
		if name, ok := entry.(string); ok && name != "" {
			metrics = append(metrics, name)
		}
	}
	if len(metrics) == 0 {
		return nil
	}
	return metrics
}

// quotaLimitedReason is a one-line explanation for a developer's log — never
// for an end user, since it describes the ACCOUNT OWNER's billing state. The
// server's own prose wins; naming the meters is the fallback, so the reason the
// SDK went quiet is never a bare boolean.
func quotaLimitedReason(parsed interface{}) string {
	if body, ok := parsed.(map[string]interface{}); ok {
		if message, ok := body["message"].(string); ok && message != "" {
			return message
		}
	}
	metrics := quotaLimitedMetrics(parsed)
	if len(metrics) == 0 {
		return "quota exhausted — this data was not stored"
	}
	return "quota exhausted for: " + strings.Join(metrics, ", ") + " — this data was not stored"
}
