package billdogeng

import (
	"context"
	"time"
)

// Llm provides LLM observability — capture a model invocation as a trace span.
//
// Wraps POST /llm/trace. The wire payload is camelCase (matching the edge
// function's Zod schema) and authenticates with X-BillDog-API-Key.
type Llm struct {
	transport *transport
	apiKey    string
}

func newLlm(t *transport, apiKey string) *Llm {
	return &Llm{transport: t, apiKey: apiKey}
}

// CaptureTrace records a single LLM trace span. out may be nil to discard the
// response.
func (l *Llm) CaptureTrace(ctx context.Context, params LlmTraceParams, out interface{}) error {
	timestamp := params.Timestamp
	if timestamp == "" {
		timestamp = time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
	}
	properties := params.Properties
	if properties == nil {
		properties = Properties{}
	}
	metadata := params.Metadata
	if metadata == nil {
		metadata = Properties{}
	}

	body := map[string]interface{}{
		"api_key":          l.apiKey,
		"traceId":          params.TraceID,
		"spanId":           params.SpanID,
		"parentSpanId":     params.ParentSpanID,
		"model":            params.Model,
		"inputText":        params.InputText,
		"outputText":       params.OutputText,
		"promptTokens":     params.PromptTokens,
		"completionTokens": params.CompletionTokens,
		"durationMs":       params.DurationMs,
		"costUsd":          params.CostUsd,
		"properties":       properties,
		"metadata":         metadata,
		"timestamp":        timestamp,
	}

	return l.transport.request(ctx, requestOptions{
		path:    "/llm/trace",
		body:    body,
		headers: map[string]string{"X-BillDog-API-Key": l.apiKey},
		noGzip:  true,
	}, out)
}
