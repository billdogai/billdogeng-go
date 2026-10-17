// Package auth provides offline verification of BillDog Auth session tokens for Go applications.
//
// NETWORKLESS BY DESIGN. Verification is a local signature check against a
// cached JWKS. It does not call BillDog on the request path, so an outage on
// our side cannot take your authorization with it, and a hot route pays no
// network cost.
//
// Ed25519 (EdDSA) verification is performed using the standard library "crypto/ed25519".
package auth

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// AuthError represents a token verification or introspection error with a canonical code.
type AuthError struct {
	Message string
	Code    string
}

func (e *AuthError) Error() string {
	return e.Message
}

// SessionClaims represents the claims inside a verified BillDog Auth session token.
type SessionClaims struct {
	Iss          string                 `json:"iss"`
	Sub          string                 `json:"sub"`
	Sid          string                 `json:"sid"`
	Aud          string                 `json:"aud"`
	Exp          int64                  `json:"exp"`
	Iat          int64                  `json:"iat"`
	Nbf          int64                  `json:"nbf"`
	Jti          string                 `json:"jti"`
	Fva          [2]int                 `json:"fva"`
	Sts          string                 `json:"sts"`
	Env          string                 `json:"env"`
	Trs          string                 `json:"trs"`
	Cus          string                 `json:"cus,omitempty"`
	Ent          []string               `json:"ent,omitempty"`
	EntTruncated bool                   `json:"ent_truncated,omitempty"`
	O            *OrgClaim              `json:"o,omitempty"`
	Act          *ActorClaim            `json:"act,omitempty"`
	Raw          map[string]interface{} `json:"-"`
}

// OrgClaim represents active organization information in a session token.
type OrgClaim struct {
	ID          string   `json:"id"`
	Slug        string   `json:"slg,omitempty"`
	Role        string   `json:"rol,omitempty"`
	Permissions []string `json:"per,omitempty"`
}

// ActorClaim represents an actor driving an impersonated or agent session.
type ActorClaim struct {
	Sub  string `json:"sub"`
	Rsn  string `json:"rsn"`
	Type string `json:"type,omitempty"`
}

// VerifyOptions configures the verification of a session token.
type VerifyOptions struct {
	ProjectID             string
	JWKSURL               string
	JWKSCacheDuration     time.Duration // Default 10 minutes
	ClockToleranceSeconds int           // Default 60 seconds
	Environment           string        // "production" (default) or "sandbox"
	Now                   *time.Time    // Override current time for deterministic testing
}

// JWK represents a JSON Web Key for Ed25519.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Kid string `json:"kid"`
	X   string `json:"x"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

type jwksResponse struct {
	Keys []JWK `json:"keys"`
}

type cachedJWKS struct {
	keys      []JWK
	fetchedAt time.Time
}

var (
	cacheMu             sync.RWMutex
	jwksCache           = make(map[string]*cachedJWKS)
	forcedRefetchAt     = make(map[string]time.Time)
	forcedRefetchMinGap = 60 * time.Second
	httpClient          = &http.Client{Timeout: 10 * time.Second}
)

func b64urlDecode(s string) ([]byte, error) {
	rem := len(s) % 4
	if rem != 0 {
		s += strings.Repeat("=", 4-rem)
	}
	return base64.URLEncoding.DecodeString(s)
}

func loadJWKS(endpoint string, cacheTTL time.Duration) ([]JWK, error) {
	now := time.Now()

	cacheMu.RLock()
	cached, ok := jwksCache[endpoint]
	cacheMu.RUnlock()

	if ok && now.Sub(cached.fetchedAt) < cacheTTL {
		return cached.keys, nil
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		if ok {
			return cached.keys, nil // Stale serving fallback
		}
		return nil, &AuthError{Message: fmt.Sprintf("Could not create JWKS request: %v", err), Code: "jwks_unavailable"}
	}
	req.Header.Set("User-Agent", "billdogeng-go")

	resp, err := httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if ok {
			return cached.keys, nil // Stale serving fallback
		}
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return nil, &AuthError{Message: fmt.Sprintf("JWKS fetch failed (status %d): %v", status, err), Code: "jwks_unavailable"}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		if ok {
			return cached.keys, nil
		}
		return nil, &AuthError{Message: "Failed to read JWKS response body", Code: "jwks_unavailable"}
	}

	var parsed jwksResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		if ok {
			return cached.keys, nil
		}
		return nil, &AuthError{Message: "Malformed JWKS response", Code: "jwks_unavailable"}
	}

	cacheMu.Lock()
	jwksCache[endpoint] = &cachedJWKS{
		keys:      parsed.Keys,
		fetchedAt: now,
	}
	cacheMu.Unlock()

	return parsed.Keys, nil
}

// VerifyToken verifies a BillDog Auth session token and returns its decoded claims.
func VerifyToken(token string, options VerifyOptions) (*SessionClaims, error) {
	if token == "" {
		return nil, &AuthError{Message: "Malformed token.", Code: "malformed"}
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, &AuthError{Message: "Malformed token.", Code: "malformed"}
	}

	headerBytes, err := b64urlDecode(parts[0])
	if err != nil {
		return nil, &AuthError{Message: "Malformed token header.", Code: "malformed"}
	}

	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, &AuthError{Message: "Malformed token header.", Code: "malformed"}
	}

	if header.Alg != "EdDSA" || header.Typ != "JWT" {
		return nil, &AuthError{Message: "Unsupported token algorithm.", Code: "bad_alg"}
	}

	if header.Kid == "" {
		return nil, &AuthError{Message: "Unknown signing key.", Code: "unknown_kid"}
	}

	baseURL := options.JWKSURL
	if baseURL == "" {
		baseURL = "https://api.billdog.io/v1/auth-jwks"
	}

	environment := options.Environment
	if environment == "" {
		environment = "production"
	}

	endpoint := fmt.Sprintf("%s?project_id=%s&environment=%s",
		baseURL, url.QueryEscape(options.ProjectID), url.QueryEscape(environment))

	cacheTTL := options.JWKSCacheDuration
	if cacheTTL <= 0 {
		cacheTTL = 10 * time.Minute
	}

	keys, err := loadJWKS(endpoint, cacheTTL)
	if err != nil {
		return nil, err
	}

	var targetKey *JWK
	for i := range keys {
		if keys[i].Kid == header.Kid {
			targetKey = &keys[i]
			break
		}
	}

	// Unknown kid: perform one forced refetch if throttled interval has elapsed
	if targetKey == nil {
		now := time.Now()
		cacheMu.RLock()
		lastRefetch := forcedRefetchAt[endpoint]
		cacheMu.RUnlock()

		if now.Sub(lastRefetch) >= forcedRefetchMinGap {
			refreshedKeys, refetchErr := loadJWKS(endpoint, 0)
			if refetchErr == nil {
				for i := range refreshedKeys {
					if refreshedKeys[i].Kid == header.Kid {
						targetKey = &refreshedKeys[i]
						break
					}
				}
			}
			if targetKey == nil {
				cacheMu.Lock()
				forcedRefetchAt[endpoint] = now
				cacheMu.Unlock()
			}
		}
	}

	if targetKey == nil {
		return nil, &AuthError{Message: "Unknown signing key.", Code: "unknown_kid"}
	}

	pubKeyBytes, err := b64urlDecode(targetKey.X)
	if err != nil || len(pubKeyBytes) != ed25519.PublicKeySize {
		return nil, &AuthError{Message: "Invalid JWK key material.", Code: "bad_signature"}
	}
	pubKey := ed25519.PublicKey(pubKeyBytes)

	sigBytes, err := b64urlDecode(parts[2])
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return nil, &AuthError{Message: "Signature verification failed.", Code: "bad_signature"}
	}

	msg := []byte(parts[0] + "." + parts[1])
	if !ed25519.Verify(pubKey, msg, sigBytes) {
		return nil, &AuthError{Message: "Signature verification failed.", Code: "bad_signature"}
	}

	claimsBytes, err := b64urlDecode(parts[1])
	if err != nil {
		return nil, &AuthError{Message: "Malformed token claims.", Code: "malformed"}
	}

	var claims SessionClaims
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return nil, &AuthError{Message: "Malformed token claims.", Code: "malformed"}
	}

	var rawClaims map[string]interface{}
	if err := json.Unmarshal(claimsBytes, &rawClaims); err == nil {
		claims.Raw = rawClaims
	}

	nowSec := time.Now().Unix()
	if options.Now != nil {
		nowSec = options.Now.Unix()
	}

	tolerance := int64(options.ClockToleranceSeconds)
	if tolerance <= 0 {
		tolerance = 60
	}

	if claims.Exp+tolerance < nowSec {
		return nil, &AuthError{Message: "Token expired.", Code: "expired"}
	}

	if claims.Nbf-tolerance > nowSec {
		return nil, &AuthError{Message: "Token not yet valid.", Code: "not_yet_valid"}
	}

	if claims.Aud != options.ProjectID {
		return nil, &AuthError{Message: "Token audience mismatch.", Code: "bad_audience"}
	}

	tokenEnv := claims.Env
	if tokenEnv == "" {
		tokenEnv = "production"
	}
	if tokenEnv != environment {
		return nil, &AuthError{
			Message: fmt.Sprintf("Token is for the %s environment, but this verifier is configured for %s.", tokenEnv, environment),
			Code:    "wrong_environment",
		}
	}

	return &claims, nil
}

// HasEntitlement returns true if the claims include the specified entitlement identifier.
func HasEntitlement(claims *SessionClaims, identifier string) bool {
	if claims == nil {
		return false
	}
	for _, ent := range claims.Ent {
		if ent == identifier {
			return true
		}
	}
	return false
}

// EntitlementsComplete returns true if the entitlement claim was not truncated.
func EntitlementsComplete(claims *SessionClaims) bool {
	if claims == nil {
		return false
	}
	return !claims.EntTruncated
}

// FactorAge returns minutes since the specified factor ('first' or 'second') was verified.
func FactorAge(claims *SessionClaims, factor string) *int {
	if claims == nil {
		return nil
	}
	idx := 0
	if factor == "second" {
		idx = 1
	}
	val := claims.Fva[idx]
	if val < 0 {
		return nil
	}
	return &val
}

// ClearJWKSCache clears the JWKS cache and forced refetch tracking (useful in tests).
func ClearJWKSCache() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	jwksCache = make(map[string]*cachedJWKS)
	forcedRefetchAt = make(map[string]time.Time)
}

// IntrospectResult represents the result from online token introspection.
type IntrospectResult struct {
	Active bool           `json:"active"`
	Claims *SessionClaims `json:"claims,omitempty"`
}

// Introspect checks online with the BillDog Auth service whether a session token is still live.
func Introspect(token, apiKey, baseURL string) (*IntrospectResult, error) {
	if baseURL == "" {
		baseURL = "https://api.billdog.io/v1"
	}

	payload, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		return nil, &AuthError{Message: fmt.Sprintf("Failed to marshal request: %v", err), Code: "introspect_failed"}
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+"/auth-introspect", bytes.NewReader(payload))
	if err != nil {
		return nil, &AuthError{Message: fmt.Sprintf("Failed to create request: %v", err), Code: "introspect_failed"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BillDog-API-Key", apiKey)
	req.Header.Set("User-Agent", "billdogeng-go")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, &AuthError{Message: fmt.Sprintf("Introspection network error: %v", err), Code: "introspect_failed"}
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &AuthError{Message: "Failed to read introspection response", Code: "introspect_failed"}
	}

	var envelope struct {
		Success bool `json:"success"`
		Data    struct {
			Active bool           `json:"active"`
			Claims *SessionClaims `json:"claims,omitempty"`
		} `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}

	if err := json.Unmarshal(bodyBytes, &envelope); err != nil {
		return nil, &AuthError{Message: "Malformed introspection response", Code: "introspect_failed"}
	}

	if !envelope.Success || resp.StatusCode >= 400 {
		code := "introspect_failed"
		msg := fmt.Sprintf("Introspection failed with status %d", resp.StatusCode)
		if envelope.Error != nil {
			if envelope.Error.Code != "" {
				code = envelope.Error.Code
			}
			if envelope.Error.Message != "" {
				msg = envelope.Error.Message
			}
		}
		return nil, &AuthError{Message: msg, Code: code}
	}

	return &IntrospectResult{
		Active: envelope.Data.Active,
		Claims: envelope.Data.Claims,
	}, nil
}
