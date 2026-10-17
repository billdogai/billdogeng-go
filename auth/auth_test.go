package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type TestVectorFile struct {
	ProjectID      string       `json:"project_id"`
	FixedNow       int64        `json:"fixed_now"`
	JWKSProduction jwksResponse `json:"jwks_production"`
	JWKSSandbox    jwksResponse `json:"jwks_sandbox"`
	Vectors        []TestVector `json:"vectors"`
}

type TestVector struct {
	Name           string                 `json:"name"`
	Token          string                 `json:"token"`
	Options        VectorOptions          `json:"options"`
	ExpectedValid  bool                   `json:"expected_valid"`
	ExpectedClaims map[string]interface{} `json:"expected_claims,omitempty"`
	ExpectedError  string                 `json:"expected_error,omitempty"`
}

type VectorOptions struct {
	ProjectID             string `json:"projectId"`
	Environment           string `json:"environment"`
	Now                   int64  `json:"now"`
	ClockToleranceSeconds int    `json:"clockToleranceSeconds,omitempty"`
}

func loadVectors(t *testing.T) *TestVectorFile {
	t.Helper()
	path := filepath.Join("..", "..", "..", "tests", "parity", "auth-token-vectors.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read test vectors at %s: %v", path, err)
	}
	var f TestVectorFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("Failed to parse test vectors: %v", err)
	}
	return &f
}

func TestAuthTokenParityVectors(t *testing.T) {
	ClearJWKSCache()
	defer ClearJWKSCache()

	vectorsFile := loadVectors(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		env := r.URL.Query().Get("environment")
		if env == "sandbox" {
			_ = json.NewEncoder(w).Encode(vectorsFile.JWKSSandbox)
		} else {
			_ = json.NewEncoder(w).Encode(vectorsFile.JWKSProduction)
		}
	}))
	defer server.Close()

	for _, v := range vectorsFile.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			nowTime := time.Unix(v.Options.Now, 0)
			tolerance := v.Options.ClockToleranceSeconds
			if tolerance == 0 {
				tolerance = 60
			}

			opts := VerifyOptions{
				ProjectID:             v.Options.ProjectID,
				JWKSURL:               server.URL,
				Environment:           v.Options.Environment,
				ClockToleranceSeconds: tolerance,
				Now:                   &nowTime,
			}

			claims, err := VerifyToken(v.Token, opts)

			if v.ExpectedValid {
				if err != nil {
					t.Fatalf("Expected valid token, got error: %v", err)
				}
				if claims.Sub != v.ExpectedClaims["sub"].(string) {
					t.Errorf("Sub mismatch: got %s, want %s", claims.Sub, v.ExpectedClaims["sub"])
				}
				if claims.Aud != v.ExpectedClaims["aud"].(string) {
					t.Errorf("Aud mismatch: got %s, want %s", claims.Aud, v.ExpectedClaims["aud"])
				}
				if claims.Env != v.ExpectedClaims["env"].(string) {
					t.Errorf("Env mismatch: got %s, want %s", claims.Env, v.ExpectedClaims["env"])
				}
			} else {
				if err == nil {
					t.Fatalf("Expected error %s, but token was valid", v.ExpectedError)
				}
				authErr, ok := err.(*AuthError)
				if !ok {
					t.Fatalf("Expected *AuthError, got %T: %v", err, err)
				}
				if authErr.Code != v.ExpectedError {
					t.Errorf("Error code mismatch: got %s, want %s (message: %s)", authErr.Code, v.ExpectedError, authErr.Message)
				}
			}
		})
	}
}

func TestClaimHelpers(t *testing.T) {
	claims := &SessionClaims{
		Sub: "user_1",
		Ent: []string{"pro", "billing_v2"},
		Fva: [2]int{10, -1},
	}

	if !HasEntitlement(claims, "pro") {
		t.Error("Expected HasEntitlement(pro) to be true")
	}
	if HasEntitlement(claims, "enterprise") {
		t.Error("Expected HasEntitlement(enterprise) to be false")
	}
	if !EntitlementsComplete(claims) {
		t.Error("Expected EntitlementsComplete to be true")
	}

	truncatedClaims := &SessionClaims{
		Sub:          "user_2",
		Ent:          []string{"pro"},
		EntTruncated: true,
		Fva:          [2]int{15, 2},
	}

	if EntitlementsComplete(truncatedClaims) {
		t.Error("Expected EntitlementsComplete to be false for truncated claims")
	}

	age1 := FactorAge(claims, "first")
	if age1 == nil || *age1 != 10 {
		t.Errorf("Expected first factor age 10, got %v", age1)
	}

	age2 := FactorAge(claims, "second")
	if age2 != nil {
		t.Errorf("Expected second factor age nil, got %v", age2)
	}

	age2Trunc := FactorAge(truncatedClaims, "second")
	if age2Trunc == nil || *age2Trunc != 2 {
		t.Errorf("Expected second factor age 2, got %v", age2Trunc)
	}
}

func TestJWKSStaleFallback(t *testing.T) {
	ClearJWKSCache()
	defer ClearJWKSCache()

	vectorsFile := loadVectors(t)
	serveFail := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveFail {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(vectorsFile.JWKSProduction)
	}))
	defer server.Close()

	v := vectorsFile.Vectors[0]
	nowTime := time.Unix(v.Options.Now, 0)
	opts := VerifyOptions{
		ProjectID:         v.Options.ProjectID,
		JWKSURL:           server.URL,
		JWKSCacheDuration: 0, // Forces check on every call
		Now:               &nowTime,
	}

	// First load succeeds and caches
	claims1, err := VerifyToken(v.Token, opts)
	if err != nil {
		t.Fatalf("First verification failed: %v", err)
	}
	if claims1.Sub != v.ExpectedClaims["sub"] {
		t.Errorf("Sub mismatch: %v", claims1.Sub)
	}

	// Now make endpoint fail
	serveFail = true

	// Should serve from stale cache without error
	claims2, err := VerifyToken(v.Token, opts)
	if err != nil {
		t.Fatalf("Expected stale fallback to succeed, got: %v", err)
	}
	if claims2.Sub != v.ExpectedClaims["sub"] {
		t.Errorf("Sub mismatch on stale fallback: %v", claims2.Sub)
	}
}
