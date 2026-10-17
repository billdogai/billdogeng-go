package billdogeng

// OPT-IN real-backend E2E smoke test.
//
// Unlike every other test in this package (which use the in-process stub
// server), this file hits the LIVE BillDog API. It is gated on
// BD_E2E_API_KEY and skips cleanly when unset, so the regular `go test`
// run stays hermetic.
//
// To run for real:
//
//	BD_E2E_API_KEY=bd_test_xxxxxxxx go test -run E2E -v
//	# optionally override the host (defaults to https://api.billdog.io/v1):
//	BD_E2E_HOST=https://api.billdog.io/v1 BD_E2E_API_KEY=bd_test_xxx go test -run E2E -v
//
// It performs a minimal real round-trip and asserts only that the live
// endpoints accept the requests (no DB-row assertions -- no DB creds needed).

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"
)

func TestE2ESmokeLiveBackend(t *testing.T) {
	apiKey := os.Getenv("BD_E2E_API_KEY")
	if apiKey == "" {
		t.Skip("BD_E2E_API_KEY not set -- skipping live E2E smoke")
	}
	host := os.Getenv("BD_E2E_HOST")
	if host == "" {
		host = "https://api.billdog.io/v1"
	}

	bd, err := New(apiKey, Options{Host: host, FlushAt: 100})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	distinctID := fmt.Sprintf("e2e-go-%d-%d", time.Now().UnixMilli(), rand.Int63())
	ctx := context.Background()

	// 1. capture + flush -- the ingest endpoint must accept the batch.
	bd.Capture(distinctID, "bd_e2e_smoke", Properties{"sdk": "go", "ts": time.Now().UnixMilli()}, nil)
	if err := bd.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// 2. feature flag fetch -- unknown flag resolves to a value, not an
	//    error, when the HTTP call succeeds.
	if _, err := bd.GetFeatureFlag(ctx, "bd_e2e_nonexistent_flag", distinctID, nil); err != nil {
		t.Fatalf("GetFeatureFlag: %v", err)
	}

	// 3. surveys.List -- the data API must return a slice (possibly empty).
	surveys, err := bd.Surveys.List(ctx, distinctID, "")
	if err != nil {
		t.Fatalf("Surveys.List: %v", err)
	}
	if surveys == nil {
		// nil slice is fine; just exercising that no error came back.
		_ = surveys
	}

	if err := bd.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
