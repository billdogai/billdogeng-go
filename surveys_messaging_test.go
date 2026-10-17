package billdogeng

import (
	"context"
	"net/http"
	"testing"
)

const (
	surveyID     = "11111111-1111-1111-1111-111111111111"
	questionID   = "22222222-2222-2222-2222-222222222222"
	choiceID     = "33333333-3333-3333-3333-333333333333"
	respondentID = "44444444-4444-4444-4444-444444444444"
	projectID    = "55555555-5555-5555-5555-555555555555"
)

func numPtr(n float64) *float64 { return &n }

// Spec test #5a: survey data round-trip.
func TestSurveyRoundTrip(t *testing.T) {
	ctx := context.Background()
	stub := startStub(func(req capturedRequest, w http.ResponseWriter, _ int) {
		switch req.Path {
		case "/bdsurvey-list":
			sendEnvelope(w, map[string]interface{}{
				"surveys": []map[string]interface{}{
					{"id": surveyID, "name": "NPS", "type": "nps", "status": "active"},
				},
			}, 200)
		case "/bdsurvey-fetch":
			sendEnvelope(w, map[string]interface{}{
				"id":          surveyID,
				"name":        "NPS",
				"should_show": true,
				"pages":       []interface{}{map[string]interface{}{"id": "p1"}},
			}, 200)
		case "/bdsurvey-submit":
			action, _ := req.Body["action"].(string)
			switch action {
			case "start":
				sendEnvelope(w, map[string]interface{}{"respondent_id": respondentID, "status": "in_progress"}, 200)
			case "partial":
				sendEnvelope(w, map[string]interface{}{"respondent_id": respondentID, "status": "in_progress", "answers_count": 1}, 200)
			case "abandon":
				sendEnvelope(w, map[string]interface{}{"respondent_id": respondentID, "status": "abandoned"}, 200)
			default:
				sendEnvelope(w, map[string]interface{}{"respondent_id": respondentID, "status": "completed", "answers_count": 1}, 200)
			}
		default:
			sendRaw(w, map[string]interface{}{}, 404)
		}
	})
	defer stub.close()

	bd, _ := New("bd_test_x", Options{Host: stub.URL, FlushAt: 100})

	list, err := bd.Surveys.List(ctx, "user-123", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != surveyID {
		t.Fatalf("list = %+v", list)
	}

	config, err := bd.Surveys.Fetch(ctx, surveyID, "user-123", "")
	if err != nil {
		t.Fatal(err)
	}
	if config["should_show"] != true || config["id"] != surveyID {
		t.Fatalf("config = %+v", config)
	}

	start, err := bd.Surveys.Start(ctx, surveyID, &SurveyContext{CustomerID: "user-123", IdempotencyKey: "idem-1"})
	if err != nil {
		t.Fatal(err)
	}
	if start.RespondentID != respondentID {
		t.Fatalf("respondent_id = %q", start.RespondentID)
	}

	answers := []SurveyAnswer{{QuestionID: questionID, ChoiceID: choiceID, AnswerNumber: numPtr(9)}}
	if err := bd.Surveys.RecordPartial(ctx, surveyID, start.RespondentID, answers, &SurveyContext{CustomerID: "user-123"}, nil); err != nil {
		t.Fatal(err)
	}

	var submitRes map[string]interface{}
	if err := bd.Surveys.Submit(ctx, surveyID, answers, &SurveyContext{
		RespondentID: start.RespondentID, CustomerID: "user-123", IdempotencyKey: "idem-1",
	}, &submitRes); err != nil {
		t.Fatal(err)
	}
	if submitRes["status"] != "completed" {
		t.Errorf("submit status = %v", submitRes["status"])
	}

	if err := bd.Surveys.Abandon(ctx, surveyID, start.RespondentID, nil); err != nil {
		t.Fatal(err)
	}

	_ = bd.Shutdown()

	submits := stub.requestsForPath("/bdsurvey-submit")
	if len(submits) != 4 {
		t.Fatalf("submit calls = %d, want 4", len(submits))
	}
	wantActions := []string{"start", "partial", "submit", "abandon"}
	for i, want := range wantActions {
		if submits[i].Body["action"] != want {
			t.Errorf("submit[%d].action = %v, want %s", i, submits[i].Body["action"], want)
		}
	}

	startBody := submits[0].Body
	if startBody["survey_id"] != surveyID || startBody["customer_id"] != "user-123" || startBody["idempotency_key"] != "idem-1" {
		t.Errorf("start body = %+v", startBody)
	}

	submitBody := submits[2].Body
	if submitBody["respondent_id"] != respondentID {
		t.Errorf("submit respondent_id = %v", submitBody["respondent_id"])
	}
	ans := submitBody["answers"].([]interface{})[0].(map[string]interface{})
	if ans["question_id"] != questionID || ans["choice_id"] != choiceID || ans["answer_number"].(float64) != 9 {
		t.Errorf("submit answer = %+v", ans)
	}

	if submits[0].Headers.Get("X-BillDog-API-Key") != "bd_test_x" {
		t.Errorf("X-BillDog-API-Key header = %q", submits[0].Headers.Get("X-BillDog-API-Key"))
	}
	if _, sent := submits[0].Headers[http.CanonicalHeaderKey("x-api-key")]; sent {
		t.Errorf("legacy x-api-key header must not be sent")
	}
}

// Spec test #5b: messaging dispatch payload shape.
func TestMessagingDispatchShape(t *testing.T) {
	ctx := context.Background()
	stub := startStub(func(req capturedRequest, w http.ResponseWriter, _ int) {
		if req.Path == "/messaging-dispatch" {
			sendEnvelope(w, map[string]interface{}{"sent": 42, "failed": 0}, 200)
			return
		}
		sendRaw(w, map[string]interface{}{}, 404)
	})
	defer stub.close()

	bd, _ := New("bd_test_x", Options{Host: stub.URL, FlushAt: 100})

	result, err := bd.Messaging.Dispatch(ctx, DispatchParams{
		ProjectID:   projectID,
		Channel:     "push",
		Content:     Properties{"title": "Hi", "body": "There"},
		Targeting:   &MessagingTargeting{Type: "all"},
		Scheduling:  &MessagingScheduling{DeliveryType: "immediate"},
		AccessToken: "jwt-token-abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result["sent"].(float64) != 42 || result["failed"].(float64) != 0 {
		t.Errorf("result = %+v", result)
	}

	_ = bd.Shutdown()

	req, ok := stub.findPath("/messaging-dispatch")
	if !ok {
		t.Fatal("no messaging-dispatch request")
	}
	b := req.Body
	if b["action"] != "send" || b["project_id"] != projectID || b["channel"] != "push" {
		t.Errorf("dispatch body = %+v", b)
	}
	content := b["content"].(map[string]interface{})
	if content["title"] != "Hi" || content["body"] != "There" {
		t.Errorf("content = %+v", content)
	}
	targeting := b["targeting"].(map[string]interface{})
	if targeting["type"] != "all" {
		t.Errorf("targeting = %+v", targeting)
	}
	scheduling := b["scheduling"].(map[string]interface{})
	if scheduling["deliveryType"] != "immediate" {
		t.Errorf("scheduling = %+v", scheduling)
	}
	if req.Headers.Get("Authorization") != "Bearer jwt-token-abc" {
		t.Errorf("Authorization = %q", req.Headers.Get("Authorization"))
	}
}

func TestLlmCaptureTraceShape(t *testing.T) {
	ctx := context.Background()
	stub := startStub(func(req capturedRequest, w http.ResponseWriter, _ int) {
		if req.Path == "/llm/trace" {
			sendEnvelope(w, map[string]interface{}{"success": true}, 200)
			return
		}
		sendRaw(w, map[string]interface{}{}, 404)
	})
	defer stub.close()

	bd, _ := New("bd_test_x", Options{Host: stub.URL, FlushAt: 100})
	if err := bd.Llm.CaptureTrace(ctx, LlmTraceParams{
		TraceID: "t-1", SpanID: "s-1", Model: "claude-opus-4-8",
		InputText: "hello", OutputText: "hi",
		PromptTokens: 10, CompletionTokens: 5, DurationMs: 123, CostUsd: 0.002,
	}, nil); err != nil {
		t.Fatal(err)
	}
	_ = bd.Shutdown()

	req, ok := stub.findPath("/llm/trace")
	if !ok {
		t.Fatal("no llm/trace request")
	}
	b := req.Body
	if b["traceId"] != "t-1" || b["spanId"] != "s-1" || b["parentSpanId"] != "" {
		t.Errorf("trace ids = %+v", b)
	}
	if b["model"] != "claude-opus-4-8" || b["inputText"] != "hello" || b["outputText"] != "hi" {
		t.Errorf("trace body = %+v", b)
	}
	if b["promptTokens"].(float64) != 10 || b["completionTokens"].(float64) != 5 || b["durationMs"].(float64) != 123 {
		t.Errorf("trace tokens = %+v", b)
	}
	if _, ok := b["timestamp"].(string); !ok {
		t.Errorf("timestamp not a string: %v", b["timestamp"])
	}
	if req.Headers.Get("X-BillDog-API-Key") != "bd_test_x" {
		t.Errorf("X-BillDog-API-Key = %q", req.Headers.Get("X-BillDog-API-Key"))
	}
}

// Local flag definitions fetched over the network (with caching).
func TestFlagDefinitionsFetchedAndCached(t *testing.T) {
	ctx := context.Background()
	stub := startStub(func(req capturedRequest, w http.ResponseWriter, _ int) {
		if req.Path == "/feature-flag-definitions" {
			sendEnvelope(w, map[string]interface{}{
				"flags": []map[string]interface{}{
					{"key": "dark_mode", "active": true, "rollout_percentage": 83},
				},
			}, 200)
			return
		}
		// Flag reads now emit `$feature_flag_called` exposure events; serve the
		// ingest flush so Shutdown's flush doesn't 404.
		if req.Path == "/ingest-events" {
			sendEnvelope(w, map[string]interface{}{"processed": 1}, 200)
			return
		}
		sendRaw(w, map[string]interface{}{}, 404)
	})
	defer stub.close()

	bd, _ := New("bd_test_x", Options{Host: stub.URL, FlushAt: 100, LocalEvaluation: true})
	// CANONICAL: murmur("flag:dark_mode:alice") % 100 = 82 < 83 -> ON
	// (see tests/fixtures/flag-bucketing.json)
	if v, _ := bd.GetFeatureFlag(ctx, "dark_mode", "alice", nil); v != true {
		t.Errorf("dark_mode = %v, want true", v)
	}
	// Second call uses cache (still one definitions fetch).
	_, _ = bd.GetFeatureFlag(ctx, "dark_mode", "bob", nil)
	defReqs := stub.requestsForPath("/feature-flag-definitions")
	if len(defReqs) != 1 {
		t.Errorf("definitions fetches = %d, want 1", len(defReqs))
	}
	_ = bd.Shutdown()
}
