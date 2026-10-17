package billdogeng

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

const gzipThresholdBytes = 1024 // only compress bodies large enough to benefit

// transportConfig configures the HTTP transport.
type transportConfig struct {
	host           string
	requestTimeout time.Duration
	gzip           bool
	maxRetries     int
	httpClient     *http.Client
	enableLogging  bool
}

// requestOptions describe a single request.
type requestOptions struct {
	// path is appended to host, e.g. "/ingest-events".
	path string
	// body is JSON-marshaled. A nil body issues a GET.
	body interface{}
	// headers are extra headers (auth headers supplied per-call).
	headers map[string]string
	// noGzip disables gzip for this request even when config.gzip is true.
	noGzip bool
}

// transport is a thin HTTP client around net/http with:
//   - per-request timeout via context
//   - optional gzip request bodies
//   - exponential backoff retry (1s, 2s, 4s ...) for 5xx / 429 / network errors
//   - unwrapping of the {success, data, meta} envelope (required on 2xx)
type transport struct {
	cfg transportConfig
}

func newTransport(cfg transportConfig) *transport {
	if cfg.httpClient == nil {
		cfg.httpClient = &http.Client{}
	}
	return &transport{cfg: cfg}
}

func (t *transport) logf(format string, args ...interface{}) {
	if t.cfg.enableLogging {
		log.Printf("[BilldogEng] "+format, args...)
	}
}

func isRetryable(status int) bool {
	return status == 0 || status == 408 || status == 429 || (status >= 500 && status <= 599)
}

// request performs a request with retry/backoff and unmarshals the (unwrapped)
// JSON response into out (which may be nil to discard the body).
func (t *transport) request(ctx context.Context, opts requestOptions, out interface{}) error {
	url := t.cfg.host + opts.path
	maxAttempts := t.cfg.maxRetries + 1

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1000*(1<<(attempt-1))) * time.Millisecond // 1s,2s,4s
			t.logf("retry %d/%d after %s -> %s", attempt, t.cfg.maxRetries, backoff, opts.path)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		err := t.attempt(ctx, url, opts, out)
		if err == nil {
			return nil
		}
		be, ok := err.(*Error)
		if !ok {
			return err
		}
		lastErr = be
		if !isRetryable(be.Status) || attempt == maxAttempts-1 {
			return be
		}
		t.logf("request failed (status %d), will retry: %s", be.Status, be.Message)
	}
	if lastErr != nil {
		return lastErr
	}
	return newError("request failed", 0, "", nil)
}

func (t *transport) attempt(ctx context.Context, url string, opts requestOptions, out interface{}) error {
	method := http.MethodGet
	var bodyReader io.Reader
	headers := map[string]string{
		"Content-Type": "application/json",
		"User-Agent":   "billdogeng-go",
	}
	for k, v := range opts.headers {
		headers[k] = v
	}

	if opts.body != nil {
		method = http.MethodPost
		jsonBytes, err := json.Marshal(opts.body)
		if err != nil {
			return newError(fmt.Sprintf("marshal error: %v", err), 0, "MARSHAL_ERROR", nil)
		}
		useGzip := t.cfg.gzip && !opts.noGzip && len(jsonBytes) >= gzipThresholdBytes
		if useGzip {
			var buf bytes.Buffer
			gw := gzip.NewWriter(&buf)
			if _, err := gw.Write(jsonBytes); err != nil {
				return newError(fmt.Sprintf("gzip error: %v", err), 0, "GZIP_ERROR", nil)
			}
			if err := gw.Close(); err != nil {
				return newError(fmt.Sprintf("gzip error: %v", err), 0, "GZIP_ERROR", nil)
			}
			headers["Content-Encoding"] = "gzip"
			bodyReader = &buf
		} else {
			bodyReader = bytes.NewReader(jsonBytes)
		}
	}

	reqCtx, cancel := context.WithTimeout(ctx, t.cfg.requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, url, bodyReader)
	if err != nil {
		return newError(fmt.Sprintf("network error: %v", err), 0, "NETWORK_ERROR", nil)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := t.cfg.httpClient.Do(req)
	if err != nil {
		// Network failure / timeout -> status 0, retryable.
		return newError(fmt.Sprintf("network error: %v", err), 0, "NETWORK_ERROR", nil)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return newError(fmt.Sprintf("read error: %v", err), 0, "NETWORK_ERROR", nil)
	}

	var parsed interface{}
	if len(raw) > 0 {
		if jErr := json.Unmarshal(raw, &parsed); jErr != nil {
			parsed = string(raw)
		}
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		msg := fmt.Sprintf("HTTP %d", res.StatusCode)
		code := ""
		if m, ok := parsed.(map[string]interface{}); ok {
			if e, ok := m["error"].(map[string]interface{}); ok {
				if s, ok := e["message"].(string); ok && s != "" {
					msg = s
				}
				if s, ok := e["code"].(string); ok {
					code = s
				}
			}
		}
		return newError(msg, res.StatusCode, code, parsed)
	}

	// A 2xx does not prove the data was stored: ingestion answers an exhausted
	// allowance with 200 and a top-level `quota_limited` array naming the drained
	// meters, so a customer's app is never broken by their meter filling up (see
	// supabase/functions/_shared/quota-limited.ts). Surfaced as a terminal,
	// non-retryable *Error — resending the same bytes cannot make room — leaving
	// the caller to decide what a refused payload means for it.
	if len(quotaLimitedMetrics(parsed)) > 0 {
		return newError(quotaLimitedReason(parsed), res.StatusCode, quotaLimitedCode, parsed)
	}

	// Every endpoint answers the standard {success, data, meta} envelope, and
	// `data` is the payload. A body carrying a `quota_limited` key with no live
	// signal (handled above) sits outside that envelope by design and holds no
	// payload. Anything else is not a BillDog answer and is refused, never read
	// as a payload.
	m, isObj := parsed.(map[string]interface{})
	if isObj {
		if _, hasQuota := m["quota_limited"]; hasQuota {
			return nil
		}
	}
	data, hasData := m["data"]
	if !isObj || m["success"] != true || !hasData {
		return newError("malformed response: expected the {success, data} envelope", res.StatusCode, "MALFORMED_RESPONSE", parsed)
	}

	if out == nil {
		return nil
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return newError(fmt.Sprintf("marshal error: %v", err), 0, "MARSHAL_ERROR", nil)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return newError(fmt.Sprintf("decode error: %v", err), res.StatusCode, "DECODE_ERROR", parsed)
	}
	return nil
}
