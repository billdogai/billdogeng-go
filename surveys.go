package billdogeng

import "context"

// Surveys is the Surveys DATA API (no UI rendering). Wraps the bdsurvey-*
// edge functions.
//
// Lifecycle: List -> Fetch -> Start -> RecordPartial* -> Submit (or Abandon).
// The same IdempotencyKey SHOULD be carried across Start and Submit so retries
// are safe.
type Surveys struct {
	transport *transport
	apiKey    string
}

func newSurveys(t *transport, apiKey string) *Surveys {
	return &Surveys{transport: t, apiKey: apiKey}
}

func (s *Surveys) authHeader() map[string]string {
	return map[string]string{"X-BillDog-API-Key": s.apiKey}
}

// List lists active surveys eligible for the (optionally identified) user.
func (s *Surveys) List(ctx context.Context, distinctID, anonymousID string) ([]SurveyListItem, error) {
	body := map[string]interface{}{"api_key": s.apiKey}
	if distinctID != "" {
		body["customer_id"] = distinctID
	}
	if anonymousID != "" {
		body["anonymous_id"] = anonymousID
	}
	var data struct {
		Surveys []SurveyListItem `json:"surveys"`
	}
	if err := s.transport.request(ctx, requestOptions{
		path: "/bdsurvey-list", body: body, headers: s.authHeader(), noGzip: true,
	}, &data); err != nil {
		return nil, err
	}
	if data.Surveys == nil {
		return []SurveyListItem{}, nil
	}
	return data.Surveys, nil
}

// Fetch fetches the full configuration for a single survey.
func (s *Surveys) Fetch(ctx context.Context, surveyID, distinctID, anonymousID string) (SurveyConfiguration, error) {
	body := map[string]interface{}{"api_key": s.apiKey, "survey_id": surveyID}
	if distinctID != "" {
		body["customer_id"] = distinctID
	}
	if anonymousID != "" {
		body["anonymous_id"] = anonymousID
	}
	var data SurveyConfiguration
	if err := s.transport.request(ctx, requestOptions{
		path: "/bdsurvey-fetch", body: body, headers: s.authHeader(), noGzip: true,
	}, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// StartResult is returned by Start.
type StartResult struct {
	RespondentID string `json:"respondent_id"`
}

// Start begins a survey response. Returns the new respondent_id.
func (s *Surveys) Start(ctx context.Context, surveyID string, sctx *SurveyContext) (StartResult, error) {
	var data StartResult
	err := s.transport.request(ctx, requestOptions{
		path:    "/bdsurvey-submit",
		body:    s.buildBody("start", surveyID, nil, sctx),
		headers: s.authHeader(),
		noGzip:  true,
	}, &data)
	return data, err
}

// RecordPartial progressively persists a partial set of answers under an
// in-progress respondent. out may be nil to discard the response.
func (s *Surveys) RecordPartial(ctx context.Context, surveyID, respondentID string, answers []SurveyAnswer, sctx *SurveyContext, out interface{}) error {
	merged := SurveyContext{}
	if sctx != nil {
		merged = *sctx
	}
	merged.RespondentID = respondentID
	return s.transport.request(ctx, requestOptions{
		path:    "/bdsurvey-submit",
		body:    s.buildBody("partial", surveyID, answers, &merged),
		headers: s.authHeader(),
		noGzip:  true,
	}, out)
}

// Submit submits the final answers, completing the response. out may be nil.
func (s *Surveys) Submit(ctx context.Context, surveyID string, answers []SurveyAnswer, sctx *SurveyContext, out interface{}) error {
	return s.transport.request(ctx, requestOptions{
		path:    "/bdsurvey-submit",
		body:    s.buildBody("submit", surveyID, answers, sctx),
		headers: s.authHeader(),
		noGzip:  true,
	}, out)
}

// Abandon marks an in-progress response as abandoned. out may be nil.
func (s *Surveys) Abandon(ctx context.Context, surveyID, respondentID string, out interface{}) error {
	merged := &SurveyContext{RespondentID: respondentID}
	return s.transport.request(ctx, requestOptions{
		path:    "/bdsurvey-submit",
		body:    s.buildBody("abandon", surveyID, nil, merged),
		headers: s.authHeader(),
		noGzip:  true,
	}, out)
}

func (s *Surveys) buildBody(action, surveyID string, answers []SurveyAnswer, sctx *SurveyContext) map[string]interface{} {
	body := map[string]interface{}{
		"action":     action,
		"api_key":    s.apiKey,
		"survey_id":  surveyID,
	}
	if answers != nil {
		body["answers"] = answers
	}
	if sctx != nil {
		if sctx.RespondentID != "" {
			body["respondent_id"] = sctx.RespondentID
		}
		if sctx.CustomerID != "" {
			body["customer_id"] = sctx.CustomerID
		}
		if sctx.AnonymousID != "" {
			body["anonymous_id"] = sctx.AnonymousID
		}
		if sctx.SessionID != "" {
			body["session_id"] = sctx.SessionID
		}
		if sctx.Platform != "" {
			body["platform"] = sctx.Platform
		}
		if sctx.DeviceInfo != nil {
			body["device_info"] = sctx.DeviceInfo
		}
		if sctx.DurationMs != nil {
			body["duration_ms"] = *sctx.DurationMs
		}
		if sctx.Context != nil {
			body["context"] = sctx.Context
		}
		if sctx.CollectorID != "" {
			body["collector_id"] = sctx.CollectorID
		}
		if sctx.CollectorType != "" {
			body["collector_type"] = sctx.CollectorType
		}
		if sctx.IdempotencyKey != "" {
			body["idempotency_key"] = sctx.IdempotencyKey
		}
	}
	return body
}
