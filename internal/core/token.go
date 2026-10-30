package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

// gatewayCaller is the caller of a request with a gateway token in straza
// mode, where Straza decides per agent.
const gatewayCaller = "straza"

// listingCaller is the caller of a request with a listing token in straza
// mode, which may call list_databases only.
const listingCaller = "straza-listing"

// tokenPrefix starts every token that adaptive-pg-mcp token makes. The token
// check does not require it.
const tokenPrefix = "apm_"

// credential is one configured token hash and the caller it names.
type credential struct {
	hash   config.Hash
	caller string
}

// credentials returns the hashes the token check accepts: those of
// gateway_tokens_sha256 and listing_tokens_sha256 in straza mode, and those
// of the callers in access.yaml in standalone mode.
func credentials(cfg *config.Config) []credential {
	var creds []credential
	if cfg.Mode == config.Straza {
		for _, h := range cfg.GatewayTokens {
			creds = append(creds, credential{hash: h, caller: gatewayCaller})
		}
		for _, h := range cfg.ListingTokens {
			creds = append(creds, credential{hash: h, caller: listingCaller})
		}
		return creds
	}
	for _, c := range cfg.Callers {
		creds = append(creds, credential{hash: c.TokenSHA256, caller: c.Name})
	}
	return creds
}

// match returns the caller whose hash is the SHA-256 of token. It compares
// the hash with every configured hash in constant time and never stops early,
// so the time it takes does not depend on which hash matches or how much of it.
func match(creds []credential, token string) (string, bool) {
	if token == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))
	found := -1
	for i := range creds {
		same := subtle.ConstantTimeCompare(sum[:], creds[i].hash[:])
		found = subtle.ConstantTimeSelect(same, i, found)
	}
	if found < 0 {
		return "", false
	}
	return creds[found].caller, true
}

// refusal says why the token check turned a request away. The zero refusal
// admits it.
type refusal int

const (
	// admitted lets the request through.
	admitted refusal = iota
	// noBearer is a request without Bearer credentials: no Authorization
	// header, or one of another scheme.
	noBearer
	// badBearer is a request whose Bearer credentials are malformed or match
	// no configured hash.
	badBearer
)

// checkBearer returns the caller of a request from the values of its
// Authorization header. The scheme is case-insensitive, as RFC 6750 allows.
func checkBearer(creds []credential, values []string) (string, refusal) {
	if len(values) == 0 {
		return "", noBearer
	}
	if len(values) > 1 {
		return "", badBearer
	}
	fields := strings.Fields(values[0])
	if len(fields) == 0 || !strings.EqualFold(fields[0], "Bearer") {
		return "", noBearer
	}
	if len(fields) != 2 {
		return "", badBearer
	}
	caller, ok := match(creds, fields[1])
	if !ok {
		return "", badBearer
	}
	return caller, admitted
}

const (
	// realm names the protection space in every challenge.
	realm = `Bearer realm="adaptive-pg-mcp"`
	// missingBearer is the response body for a request without a bearer token.
	missingBearer = "the request has no bearer token. Send the header Authorization: Bearer followed by the token issued for this server."
	// invalidBearer is the response body for a malformed or unknown bearer
	// token.
	invalidBearer = "the bearer token is malformed or unknown to this server. Send the token issued for this server, once, after Bearer."
)

// checkedCallerKey carries the caller the token check found to the SDK's
// token verifier.
type checkedCallerKey struct{}

// authenticate runs the token check on every HTTP request. A refusal gets
// 401 with a challenge and a message, and the token is never logged or
// repeated. On a match it hands the caller to the SDK's bearer middleware,
// which puts it in TokenInfo.UserID for session binding and the request's
// handlers, and it removes the Authorization header so that nothing past the
// check sees it.
func (s *Server) authenticate(next http.Handler) http.Handler {
	verify := func(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
		caller, ok := ctx.Value(checkedCallerKey{}).(string)
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: caller}, nil
	}
	bound := auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(withoutAuthorization(next))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, why := checkBearer(s.creds, r.Header.Values("Authorization"))
		if why != admitted {
			challenge, body := realm, missingBearer
			if why == badBearer {
				challenge, body = realm+`, error="invalid_token"`, invalidBearer
			}
			s.log.Warn("refused an HTTP request that has no valid bearer token", "remote", r.RemoteAddr, "method", r.Method)
			w.Header().Set("WWW-Authenticate", challenge)
			http.Error(w, body, http.StatusUnauthorized)
			return
		}
		bound.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), checkedCallerKey{}, caller)))
	})
}

// withoutAuthorization removes the Authorization header before next, so that
// no handler past the token check can log or forward the token.
func withoutAuthorization(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.Clone(r.Context())
		r.Header.Del("Authorization")
		next.ServeHTTP(w, r)
	})
}

// NewToken makes a token from 32 bytes of crypto/rand, written as apm_ and
// unpadded base64url, and returns it with its SHA-256 in lowercase hex.
func NewToken() (token, sha256Hex string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("adaptive-pg-mcp could not read random bytes for a token: %v. Try again, and check the system's random source if it fails twice.", err)
	}
	token = tokenPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}
