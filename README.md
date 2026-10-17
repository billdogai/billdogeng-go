# billdogeng-go

Official **BilldogEng** server SDK for Go — the engagement suite for server-side
use: Analytics, Feature Flags (remote + local evaluation), Surveys (data API),
Messaging dispatch, and LLM observability.

This is a faithful port of the canonical Node reference SDK (`native-sdks/node`)
and conforms to the cross-language [server SDK spec](../../scratch/server-sdk-spec.md).
The wire format and method surface are identical across all language SDKs.

## Install

```sh
go get github.com/billdogai/billdogeng-go@v1.1.1-beta.3
```

```go
import billdogeng "github.com/billdogai/billdogeng-go"
```

## Quickstart

```go
package main

import (
	"context"
	"log"

	billdogeng "github.com/billdogai/billdogeng-go"
)

func main() {
	bd, err := billdogeng.New("bd_test_xxx", billdogeng.Options{
		LocalEvaluation: true,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer bd.Shutdown() // flush + stop the background flush timer

	ctx := context.Background()

	// Analytics — batched, flushed on FlushAt / FlushInterval / Shutdown.
	bd.Capture("user-123", "order_completed", billdogeng.Properties{"revenue": 49.99}, nil)
	bd.Identify("user-123", billdogeng.Properties{"plan": "pro"})
	bd.GroupIdentify("company", "acme", billdogeng.Properties{"seats": 50})
	bd.Alias("user-123", "anon-abc")

	// Feature flags (local evaluation — deterministic murmurhash3 bucketing).
	on, _ := bd.IsFeatureEnabled(ctx, "new_checkout", "user-123", nil)
	variant, _ := bd.GetFeatureFlag(ctx, "pricing_test", "user-123", &billdogeng.FlagEvalOptions{
		PersonProperties: billdogeng.Properties{"plan": "pro"},
	})
	_ = on
	_ = variant

	// Surveys data API.
	list, _ := bd.Surveys.List(ctx, "user-123", "")
	if len(list) > 0 {
		start, _ := bd.Surveys.Start(ctx, list[0].ID, &billdogeng.SurveyContext{CustomerID: "user-123"})
		_ = bd.Surveys.Submit(ctx, list[0].ID, []billdogeng.SurveyAnswer{
			{QuestionID: "q1", AnswerText: "great"},
		}, &billdogeng.SurveyContext{RespondentID: start.RespondentID}, nil)
	}

	// Messaging dispatch (Bearer JWT auth, not X-BillDog-API-Key).
	bd.Messaging.Dispatch(ctx, billdogeng.DispatchParams{
		ProjectID:   "project-uuid",
		Channel:     "push",
		Content:     billdogeng.Properties{"title": "Hi", "body": "There"},
		Targeting:   &billdogeng.MessagingTargeting{Type: "all"},
		Scheduling:  &billdogeng.MessagingScheduling{DeliveryType: "immediate"},
		AccessToken: "supabase-session-jwt",
	})

	// LLM observability.
	bd.Llm.CaptureTrace(ctx, billdogeng.LlmTraceParams{
		TraceID: "t-1", SpanID: "s-1", Model: "claude-opus-4-8",
		InputText: "hello", OutputText: "hi",
		PromptTokens: 10, CompletionTokens: 5, DurationMs: 123, CostUsd: 0.002,
	}, nil)
}
```

## Configuration (`Options`)

| Field              | Default                       | Description                                          |
| ------------------ | ----------------------------- | ---------------------------------------------------- |
| `Host`             | `https://api.billdog.io/v1`   | Base URL for all requests.                           |
| `FlushAt`          | `20`                          | Batch size that triggers an auto-flush.              |
| `FlushIntervalMs`  | `10000`                       | Background flush cadence (ms).                       |
| `MaxQueueSize`     | `1000`                        | Max queued events before oldest are dropped.         |
| `Gzip`             | `true`                        | Gzip large request bodies. Use `Options{}.WithGzip(false)` to disable. |
| `LocalEvaluation`  | `false`                       | Enable local (server-side) feature-flag evaluation.  |
| `RequestTimeoutMs` | `10000`                       | Per-request timeout (ms).                            |
| `MaxRetries`       | `3`                           | Retry attempts for 5xx / network errors.             |
| `EnableLogging`    | `false`                       | Verbose diagnostics to stderr.                       |

Auth: every request sends `X-BillDog-API-Key: <apiKey>` (`bd_test_*` sandbox /
`bd_live_*` live), except messaging-dispatch which uses a Bearer JWT, and the
LLM endpoint which uses `X-BillDog-API-Key`.

## Quota limits (`quota_limited`)

Ingestion answers an exhausted allowance with **HTTP 200** and a top-level
`quota_limited` array naming the drained meters — never an error status — so a
customer's app is never broken by their BillDog meter filling up. The data is
NOT stored, so the SDK does not treat that 200 as success:

- `Flush()` returns an error matching `errors.Is(err, ErrQuotaLimited)`; the
  refused batch is dropped, not re-queued (the server would refuse it again).
- Flushing suspends and `Capture`/`Identify`/`GroupIdentify`/`Alias` become
  no-ops, so a long-running process cannot grow a queue that can never drain.
- The reason — the server's own message — is logged ONCE at warn level,
  regardless of `EnableLogging`. It is the only signal that the pipeline is dark.
- After 15 minutes one probe flush re-tests the meter, since an allowance resets
  monthly and a plan can be upgraded at any time. On success the SDK resumes and
  logs the recovery once; otherwise it stays dark for another 15 minutes.

## Feature-flag local evaluation

With `LocalEvaluation: true` the SDK fetches flag definitions once from
`POST /feature-flag-definitions`, caches them (5-minute TTL), and evaluates each
flag deterministically:

1. Missing/inactive → `false`.
2. The flag's `targeting_rule` must match, else `false`. It arrives in the ONE
   canonical audience DSL (`{status, enabled, start_date, end_date, segment_ids,
   conditions: {logic, rules}}`) and is resolved locally by
   `EvaluateTargetingRule` in `targeting.go` — AND/OR, date windows, rule status,
   the full operator set and **group membership** all resolve with no server
   round-trip. Context comes from `FlagEvalOptions` (`Groups`, `PersonProperties`,
   `Country`, `Platform`, `AppVersion`, `SdkVersion`, `ExperimentVariants`);
   `Country`/`Platform`/`AppVersion`/`SdkVersion` fall back to a same-named person
   property.
3. `bucket = murmurhash3("flag:{key}:{distinctId}") % 100`; ON iff `bucket < rollout_percentage`.
4. Multivariate: walk `variants` by cumulative rollout against an INDEPENDENT hash,
   `murmurhash3("flag-variant:{key}:{distinctId}") % 100` — not the rollout bucket.

DB-backed conditions (`segment`, `cohort`, `survey_answer`,
`event_fired_in_window`, `group_property`) cannot be answered in-process. The
backend marks such flags `requires_server_evaluation`; if one still reaches the
local evaluator it goes **COLD** (`EvaluateLocally` returns `cold == true`) and the
SDK asks the server rather than guessing — a COLD verdict is never collapsed into
`false`. `GetAllFlags` batches every server-needing flag into ONE request.

The `Murmurhash3` (32-bit, seed 0, UTF-8 bytes) function is identical to the
web / iOS / Android implementations, so a user is deterministically in or out of
a rollout regardless of which platform evaluates the flag.

## Testing

```sh
go test ./... -count=1
```

Covered: batching (10 captures → 1 POST of 10 events), identify/group/alias event
shapes, the 12 canonical murmurhash3 vectors + rollout/targeting/multivariate
evaluation, 503-then-200 retry with backoff against a local stub server, and the
survey round-trip + messaging/LLM payload shapes.

Two conformance suites pin this SDK to the BACKEND (never to a sibling SDK — nine
hand-ports of the murmur hash once all agreed with each other and all disagreed
with the server):

- `conformance_test.go` → `tests/fixtures/flag-bucketing.json` (rollout + variant bucketing)
- `targeting_conformance_test.go` → `tests/fixtures/flag-targeting.json` (145 rule × context vectors,
  the COLD contract, and local group membership)

## BillDog Auth

Session-token verification is **not available in this SDK yet**.
[`billdogeng-node`](https://www.npmjs.com/package/billdogeng-node) is the only
server SDK that can verify a BillDog Auth token offline today.

Until it lands here, either verify in a Node service, or call the
`auth-introspect` endpoint over HTTP — that costs a network round-trip per
check, and in exchange tells you whether the session is live *right now* rather
than only that the token was valid when it was issued.

## License

MIT
