// Who a /mcp request acts as. Auth mode none binds one tenant at startup; jwt mode takes the
// tenant from an HS256 token on every request, so one server serves many tenants.
package server

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// minSecret is HS256's key size; a shorter secret is guessable offline from any one token.
const minSecret = 32

type caller struct {
	tenant uuid.UUID
	actor  string
}

type callerKey struct{}

func withCaller(next http.Handler, c caller) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
	})
}

// FixedTenant serves every request as tenant, for auth mode none.
func FixedTenant(tenant uuid.UUID) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler { return withCaller(next, caller{tenant, actor}) }
}

// JWT verifies HS256 tokens from one trusted issuer. Secrets[0] signs; every secret verifies, for rotation.
type JWT struct {
	Issuer, Audience string
	Secrets          [][]byte
}

// JWTFromEnv reads KEEPSAKE_JWT_ISSUER, KEEPSAKE_JWT_AUDIENCE and one secret per line of KEEPSAKE_JWT_SECRET_FILE.
func JWTFromEnv() (*JWT, error) {
	j := &JWT{Issuer: os.Getenv("KEEPSAKE_JWT_ISSUER"), Audience: os.Getenv("KEEPSAKE_JWT_AUDIENCE")}
	if j.Issuer == "" || j.Audience == "" {
		return nil, errors.New("jwt mode needs KEEPSAKE_JWT_ISSUER and KEEPSAKE_JWT_AUDIENCE")
	}
	b, err := os.ReadFile(os.Getenv("KEEPSAKE_JWT_SECRET_FILE"))
	if err != nil {
		return nil, fmt.Errorf("jwt mode needs KEEPSAKE_JWT_SECRET_FILE: %w", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if len(line) < minSecret {
			return nil, fmt.Errorf("every secret in KEEPSAKE_JWT_SECRET_FILE must be at least %d bytes", minSecret)
		}
		j.Secrets = append(j.Secrets, []byte(line))
	}
	if len(j.Secrets) == 0 {
		return nil, errors.New("KEEPSAKE_JWT_SECRET_FILE holds no secret")
	}
	return j, nil
}

var b64 = base64.RawURLEncoding

// Mint signs a token for tenant, acting as sub, that expires after ttl.
func (j *JWT) Mint(tenant uuid.UUID, sub string, ttl time.Duration) string {
	now := time.Now().Unix()
	payload, err := json.Marshal(map[string]any{"iss": j.Issuer, "aud": j.Audience, "sub": sub, "iat": now,
		"exp": now + int64(ttl/time.Second), "tctx": map[string]string{"tenant": tenant.String()}})
	if err != nil {
		panic(err)
	}
	unsigned := b64.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + b64.EncodeToString(payload)
	return unsigned + "." + b64.EncodeToString(mac(j.Secrets[0], unsigned))
}

// audience is aud as RFC 7519 allows it: one string or a list.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	return json.Unmarshal(b, (*[]string)(a))
}

type claimSet struct {
	Iss  string          `json:"iss"`
	Sub  string          `json:"sub"`
	Aud  audience        `json:"aud"`
	Exp  *float64        `json:"exp"`
	Nbf  *float64        `json:"nbf"`
	Tctx json.RawMessage `json:"tctx"`
}

var errForbidden = errors.New("no tenant in the token")

// verify returns the token's caller, errForbidden for a genuine token naming no usable tenant, or another error.
func (j *JWT) verify(token string) (caller, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return caller{}, errors.New("not a JWS compact token")
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return caller{}, errors.New("signature is not base64url")
	}
	var header struct{ Alg string }
	if h, err := b64.DecodeString(parts[0]); err != nil || json.Unmarshal(h, &header) != nil || header.Alg != "HS256" {
		return caller{}, errors.New("alg is not HS256")
	}
	signed := false
	for _, s := range j.Secrets {
		signed = signed || hmac.Equal(sig, mac(s, parts[0]+"."+parts[1]))
	}
	if !signed {
		return caller{}, errors.New("bad signature")
	}
	var c claimSet
	if p, err := b64.DecodeString(parts[1]); err != nil || json.Unmarshal(p, &c) != nil {
		return caller{}, errors.New("claims are not a JSON object")
	}
	now := float64(time.Now().Unix())
	switch {
	case c.Iss != j.Issuer:
		return caller{}, errors.New("wrong iss")
	case !slices.Contains(c.Aud, j.Audience):
		return caller{}, errors.New("wrong aud")
	case c.Exp == nil || now >= *c.Exp:
		return caller{}, errors.New("expired or no exp")
	case c.Nbf != nil && now < *c.Nbf:
		return caller{}, errors.New("not yet valid")
	case c.Sub == "":
		return caller{}, errors.New("no sub")
	}
	var tctx struct{ Tenant string }
	if json.Unmarshal(c.Tctx, &tctx) != nil {
		return caller{}, errForbidden
	}
	tenant, err := uuid.Parse(tctx.Tenant)
	if err != nil || tenant == uuid.Nil {
		return caller{}, errForbidden
	}
	return caller{tenant, c.Sub}, nil
}

// Middleware binds the verified caller, or answers 401 for a bad token and 403 for a token naming no tenant.
func (j *JWT) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := j.bearer(r)
		if err == nil {
			withCaller(next, c).ServeHTTP(w, r)
			return
		}
		// The reason is logged, never returned, and never includes the token.
		slog.Warn("refused /mcp request", "reason", err.Error())
		if errors.Is(err, errForbidden) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	})
}

func (j *JWT) bearer(r *http.Request) (caller, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return caller{}, errors.New("want exactly one Authorization header")
	}
	scheme, token, _ := strings.Cut(values[0], " ")
	if !strings.EqualFold(scheme, "Bearer") || strings.ContainsAny(token, " \t") || token == "" {
		return caller{}, errors.New("not a bearer token")
	}
	return j.verify(token)
}
