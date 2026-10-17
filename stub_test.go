package billdogeng

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// capturedRequest records one request received by the stub server.
type capturedRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Body    map[string]interface{}
	RawBody string
}

// stubHandler receives the parsed request, the ResponseWriter, and a 0-based hit
// counter so a test can vary behavior per-attempt (e.g. 503 then 200).
type stubHandler func(req capturedRequest, w http.ResponseWriter, hits int)

// stubServer is a local HTTP server for resilience / round-trip tests (permitted
// by the spec for these tests only).
type stubServer struct {
	srv      *httptest.Server
	URL      string
	mu       sync.Mutex
	requests []capturedRequest
	hits     int
}

func startStub(handler stubHandler) *stubServer {
	s := &stubServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" && len(raw) > 1 && raw[0] == 0x1f && raw[1] == 0x8b {
			gr, err := gzip.NewReader(bytes.NewReader(raw))
			if err == nil {
				if dec, err := io.ReadAll(gr); err == nil {
					raw = dec
				}
			}
		}
		var body map[string]interface{}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		captured := capturedRequest{
			Method:  r.Method,
			Path:    r.URL.Path,
			Headers: r.Header.Clone(),
			Body:    body,
			RawBody: string(raw),
		}
		s.mu.Lock()
		s.requests = append(s.requests, captured)
		current := s.hits
		s.hits++
		s.mu.Unlock()
		handler(captured, w, current)
	}))
	s.URL = s.srv.URL
	return s
}

func (s *stubServer) close() { s.srv.Close() }

func (s *stubServer) requestsForPath(path string) []capturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []capturedRequest
	for _, r := range s.requests {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (s *stubServer) findPath(path string) (capturedRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.requests {
		if r.Path == path {
			return r, true
		}
	}
	return capturedRequest{}, false
}

// sendEnvelope responds with a {success, data, meta} envelope (status 200).
func sendEnvelope(w http.ResponseWriter, data interface{}, status int) {
	if status == 0 {
		status = 200
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": status < 400,
		"data":    data,
		"meta":    map[string]interface{}{"request_id": "stub"},
	})
}

// sendRaw responds with a raw JSON body (error statuses and non-envelope negatives).
func sendRaw(w http.ResponseWriter, body interface{}, status int) {
	if status == 0 {
		status = 200
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
