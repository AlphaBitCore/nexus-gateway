package middleware

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestJWKSCache_Refresh_NewRequestError covers the `NewRequestWithContext`
// error branch — a URL containing a control character (NUL) is rejected
// by net/http's URL parser inside the request builder. Defensive but
// reachable, so we lock the wrap message.
func TestJWKSCache_Refresh_NewRequestError(t *testing.T) {
	c := NewJWKSCache("http://\x00", slog.Default())
	err := c.refresh()
	if err == nil || !strings.Contains(err.Error(), "build JWKS request") {
		t.Errorf("err=%v, want 'build JWKS request'", err)
	}
}

// TestJWKSCache_Refresh_BodyReadError covers the `io.ReadAll` error
// branch — the JWKS endpoint returns 200, advertises a Content-Length,
// then hijacks the connection and closes it mid-body. ReadAll surfaces
// io.ErrUnexpectedEOF, which the refresh path must wrap as
// "read JWKS body".
func TestJWKSCache_Refresh_BodyReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Skip("response writer doesn't support hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Logf("hijack: %v", err)
			return
		}
		_ = conn.Close() // close before writing the 100 bytes
	}))
	t.Cleanup(srv.Close)
	c := NewJWKSCache(srv.URL, slog.Default())
	err := c.refresh()
	if err == nil || !strings.Contains(err.Error(), "read JWKS body") {
		t.Errorf("err=%v, want 'read JWKS body'", err)
	}
}

// TestJWKSCache_GetKey_RefreshError covers the refresh-failure branch
// of GetKey — when the JWKS endpoint is unreachable the cache must
// surface a wrapped "jwks refresh" error, not silently return a stale
// or zero key.
func TestJWKSCache_GetKey_RefreshError(t *testing.T) {
	// Point the cache at a port that's guaranteed to refuse to avoid
	// the default DialContext fallback dance. Using an httptest
	// server we immediately close gives us a stable refused-connection
	// failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	srv.Close() // close so subsequent requests fail

	c := NewJWKSCache(srv.URL, slog.Default())
	if _, err := c.GetKey("any"); err == nil || !strings.Contains(err.Error(), "jwks refresh") {
		t.Errorf("err=%v, want 'jwks refresh' wrap", err)
	}
}

// TestJWKSCache_GetKey_UnknownKid covers the "refresh succeeded but
// kid still missing" branch — the IdP rotated keys but the token
// header references an old kid that's no longer published.
func TestJWKSCache_GetKey_UnknownKid(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA",
					"kid": "current-kid",
					"alg": "RS256",
					"use": "sig",
					"n":   base64.RawURLEncoding.EncodeToString(priv.N.Bytes()),
					"e":   base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
				},
			},
		})
	}))
	t.Cleanup(srv.Close)
	c := NewJWKSCache(srv.URL, slog.Default())
	if _, err := c.GetKey("rotated-out-kid"); err == nil || !strings.Contains(err.Error(), "not found in JWKS") {
		t.Errorf("err=%v, want 'not found in JWKS'", err)
	}
}

// TestJWKSCache_GetKey_CachedHit covers the fast-path early return:
// after one successful refresh, a second GetKey for the same kid must
// NOT call the JWKS endpoint again (cache TTL is 10 minutes by default).
func TestJWKSCache_GetKey_CachedHit(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	var fetches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA",
					"kid": "k1",
					"alg": "RS256",
					"use": "sig",
					"n":   base64.RawURLEncoding.EncodeToString(priv.N.Bytes()),
					"e":   base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
				},
			},
		})
	}))
	t.Cleanup(srv.Close)
	c := NewJWKSCache(srv.URL, slog.Default())
	if _, err := c.GetKey("k1"); err != nil {
		t.Fatalf("first GetKey: %v", err)
	}
	if _, err := c.GetKey("k1"); err != nil {
		t.Fatalf("second GetKey: %v", err)
	}
	if fetches != 1 {
		t.Errorf("fetches=%d, want 1 — TTL cache not honoured", fetches)
	}
}

// TestJWKSCache_Refresh_Non200 covers the `resp.StatusCode != 200`
// branch — a misconfigured IdP returns 404/500 on its JWKS URI and we
// must surface the status code in the error message.
func TestJWKSCache_Refresh_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	c := NewJWKSCache(srv.URL, slog.Default())
	err := c.refresh()
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("err=%v, want 403 in message", err)
	}
}

// TestJWKSCache_Refresh_BadJSON covers the json.Unmarshal failure
// branch — the JWKS endpoint returned 200 but with garbage body.
func TestJWKSCache_Refresh_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	}))
	t.Cleanup(srv.Close)
	c := NewJWKSCache(srv.URL, slog.Default())
	err := c.refresh()
	if err == nil || !strings.Contains(err.Error(), "parse JWKS") {
		t.Errorf("err=%v, want parse JWKS wrap", err)
	}
}

// TestJWKSCache_Refresh_SkipsNonRSAAndMissingKid covers the
// continue branches in the key-load loop — non-RSA kty and empty kid
// must be silently skipped without aborting the whole refresh.
func TestJWKSCache_Refresh_SkipsNonRSAAndMissingKid(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{"kty": "EC", "kid": "ec1"}, // non-RSA → skip
				{"kty": "RSA", "kid": ""},   // missing kid → skip
				{"kty": "RSA", "kid": "broken", "n": "%%%not-b64%%%", "e": "AQAB"}, // bad n → warn-skip
				{ // valid one
					"kty": "RSA",
					"kid": "good",
					"alg": "RS256",
					"n":   base64.RawURLEncoding.EncodeToString(priv.N.Bytes()),
					"e":   base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
				},
			},
		})
	}))
	t.Cleanup(srv.Close)
	c := NewJWKSCache(srv.URL, slog.Default())
	if err := c.refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, ok := c.keys["good"]; !ok {
		t.Error("good kid missing — refresh aborted on bad sibling")
	}
	if _, ok := c.keys[""]; ok {
		t.Error("empty kid was loaded — should have been skipped")
	}
	if _, ok := c.keys["ec1"]; ok {
		t.Error("EC kid was loaded — should have been skipped")
	}
	if _, ok := c.keys["broken"]; ok {
		t.Error("broken-n kid was loaded — should have been skipped")
	}
}

// TestParseRSAPublicKey_Errors covers the three failure branches in
// parseRSAPublicKey: bad n base64, bad e base64, and exponent too
// large (won't fit in int64).
func TestParseRSAPublicKey_Errors(t *testing.T) {
	t.Run("bad_n_base64", func(t *testing.T) {
		_, err := parseRSAPublicKey("%%%bad%%%", "AQAB")
		if err == nil || !strings.Contains(err.Error(), "decode n") {
			t.Errorf("err=%v, want 'decode n' wrap", err)
		}
	})
	t.Run("bad_e_base64", func(t *testing.T) {
		_, err := parseRSAPublicKey("AQAB", "%%%bad%%%")
		if err == nil || !strings.Contains(err.Error(), "decode e") {
			t.Errorf("err=%v, want 'decode e' wrap", err)
		}
	})
	t.Run("exponent_too_large", func(t *testing.T) {
		// 9 bytes of 0xFF — definitely > int64 max.
		bigE := base64.RawURLEncoding.EncodeToString([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
		_, err := parseRSAPublicKey("AQAB", bigE)
		if err == nil || !strings.Contains(err.Error(), "exponent too large") {
			t.Errorf("err=%v, want 'exponent too large'", err)
		}
	})
}

// TestValidateJWT_StructuralErrors covers the structural-parse
// branches: wrong number of parts, bad header b64, bad header JSON,
// non-RS256 alg, bad signature b64, missing exp, non-numeric exp,
// bad payload b64/JSON, missing sub, default email/group claim names.
func TestValidateJWT_StructuralErrors(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "k"
	srv := startJWKSServer(t, kid, &priv.PublicKey)
	cfg := &OidcConfig{
		Issuer:   "https://i",
		JwksUri:  srv.URL,
		Audience: "a",
	}
	cache := NewJWKSCache(srv.URL, slog.Default())

	cases := []struct {
		name    string
		token   string
		wantSub string
	}{
		{"wrong_parts", "a.b", "expected 3 parts"},
		{"bad_header_b64", "%%%.b.c", "decode JWT header"},
		{"bad_header_json", base64.RawURLEncoding.EncodeToString([]byte("not json")) + ".b.c", "parse JWT header"},
	}
	for _, tc := range cases {

		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateJWT(tc.token, cfg, cache)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err=%v, want substring %q", err, tc.wantSub)
			}
		})
	}

	// non-RS256 alg
	t.Run("non_rs256_alg", func(t *testing.T) {
		hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","kid":"k"}`))
		token := hdr + ".eyJzdWIiOiJ1In0.sig"
		_, err := ValidateJWT(token, cfg, cache)
		if err == nil || !strings.Contains(err.Error(), "unsupported JWT algorithm") {
			t.Errorf("err=%v, want 'unsupported JWT algorithm'", err)
		}
	})

	// kid unknown
	t.Run("unknown_kid", func(t *testing.T) {
		hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"missing"}`))
		token := hdr + ".eyJzdWIiOiJ1In0.sig"
		_, err := ValidateJWT(token, cfg, cache)
		if err == nil || !strings.Contains(err.Error(), "get signing key") {
			t.Errorf("err=%v, want 'get signing key' wrap", err)
		}
	})

	// bad signature b64 — header + payload parse fine, but sig is %%%.
	t.Run("bad_sig_b64", func(t *testing.T) {
		token := buildJWT(t,
			map[string]any{"alg": "RS256", "kid": kid},
			map[string]any{"sub": "u", "iss": "https://i", "aud": "a", "exp": float64(time.Now().Add(time.Hour).Unix())},
			priv,
		)
		// Replace the signature segment with garbage.
		parts := strings.Split(token, ".")
		bad := parts[0] + "." + parts[1] + ".%%%"
		_, err := ValidateJWT(bad, cfg, cache)
		if err == nil || !strings.Contains(err.Error(), "decode JWT signature") {
			t.Errorf("err=%v, want 'decode JWT signature'", err)
		}
	})

	// payload segment is unparseable b64 — must surface as
	// "decode JWT payload" wrap, distinct from the json-parse wrap.
	t.Run("bad_payload_b64", func(t *testing.T) {
		// Build a real header.payload pair, sign it, then mutate the
		// payload b64 segment to contain a char that breaks
		// RawURLEncoding.DecodeString.
		hdrB64 := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"` + kid + `"}`))
		badPayload := "@@@not-b64@@@"
		token := reSign(t, hdrB64, badPayload, priv)
		_, err := ValidateJWT(token, cfg, cache)
		if err == nil || !strings.Contains(err.Error(), "decode JWT payload") {
			t.Errorf("err=%v, want 'decode JWT payload'", err)
		}
	})

	// payload not JSON (legal b64 but content garbage)
	t.Run("bad_payload_json", func(t *testing.T) {
		hdr := buildJWT(t, map[string]any{"alg": "RS256", "kid": kid}, map[string]any{"sub": "u"}, priv)
		parts := strings.Split(hdr, ".")
		parts[1] = base64.RawURLEncoding.EncodeToString([]byte("not json"))
		// Rebuild and re-sign so signature passes.
		token := reSign(t, parts[0], parts[1], priv)
		_, err := ValidateJWT(token, cfg, cache)
		if err == nil || !strings.Contains(err.Error(), "parse JWT payload") {
			t.Errorf("err=%v, want 'parse JWT payload'", err)
		}
	})

	// payload missing exp
	t.Run("missing_exp", func(t *testing.T) {
		token := buildJWT(t,
			map[string]any{"alg": "RS256", "kid": kid},
			map[string]any{"sub": "u", "iss": "https://i", "aud": "a"},
			priv,
		)
		_, err := ValidateJWT(token, cfg, cache)
		if err == nil || !strings.Contains(err.Error(), "missing exp") {
			t.Errorf("err=%v, want 'missing exp'", err)
		}
	})

	// exp is a string, not a number
	t.Run("exp_not_number", func(t *testing.T) {
		token := buildJWT(t,
			map[string]any{"alg": "RS256", "kid": kid},
			map[string]any{"sub": "u", "iss": "https://i", "aud": "a", "exp": "not-a-number"},
			priv,
		)
		_, err := ValidateJWT(token, cfg, cache)
		if err == nil || !strings.Contains(err.Error(), "exp claim is not a number") {
			t.Errorf("err=%v, want 'exp claim is not a number'", err)
		}
	})

	// missing sub
	t.Run("missing_sub", func(t *testing.T) {
		token := buildJWT(t,
			map[string]any{"alg": "RS256", "kid": kid},
			map[string]any{"iss": "https://i", "aud": "a", "exp": float64(time.Now().Add(time.Hour).Unix())},
			priv,
		)
		_, err := ValidateJWT(token, cfg, cache)
		if err == nil || !strings.Contains(err.Error(), "missing sub") {
			t.Errorf("err=%v, want 'missing sub'", err)
		}
	})
}

// reSign rebuilds a JWT after the caller swapped payload contents:
// header.payload are joined and signed with the private key.
func reSign(t *testing.T, headerB64, payloadB64 string, priv *rsa.PrivateKey) string {
	t.Helper()
	sigInput := headerB64 + "." + payloadB64
	hashed := sha256.Sum256([]byte(sigInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatal(err)
	}
	return sigInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// TestValidateJWT_DefaultClaimNamesAndGroupShapes covers the
// fallback paths in the ValidateJWT claim extractor: when
// config.EmailClaim / config.GroupClaim are empty the function
// defaults to "email" / "groups"; groups can arrive as a single
// string OR an []any of strings; non-string elements in an
// []any group claim are silently dropped (defensive).
func TestValidateJWT_DefaultClaimNamesAndGroupShapes(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "kdef"
	srv := startJWKSServer(t, kid, &priv.PublicKey)
	cfg := &OidcConfig{
		Issuer:   "https://i",
		JwksUri:  srv.URL,
		Audience: "a",
		// EmailClaim, GroupClaim deliberately empty → defaults apply.
	}
	cache := NewJWKSCache(srv.URL, slog.Default())

	t.Run("groups_as_single_string", func(t *testing.T) {
		token := buildJWT(t,
			map[string]any{"alg": "RS256", "kid": kid},
			map[string]any{
				"sub":    "u-1",
				"iss":    "https://i",
				"aud":    "a",
				"exp":    float64(time.Now().Add(time.Hour).Unix()),
				"email":  "u@x",
				"groups": "admins",
			},
			priv,
		)
		c, err := ValidateJWT(token, cfg, cache)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if c.Email != "u@x" {
			t.Errorf("Email=%q want u@x", c.Email)
		}
		if len(c.Groups) != 1 || c.Groups[0] != "admins" {
			t.Errorf("Groups=%v want [admins]", c.Groups)
		}
	})

	t.Run("groups_array_with_nonstring_skipped", func(t *testing.T) {
		token := buildJWT(t,
			map[string]any{"alg": "RS256", "kid": kid},
			map[string]any{
				"sub":    "u-2",
				"iss":    "https://i",
				"aud":    "a",
				"exp":    float64(time.Now().Add(time.Hour).Unix()),
				"groups": []any{"admins", 42, "viewers"},
			},
			priv,
		)
		c, err := ValidateJWT(token, cfg, cache)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		// 42 must be silently dropped → 2 strings remain.
		if len(c.Groups) != 2 {
			t.Errorf("Groups=%v want 2 strings (non-string dropped)", c.Groups)
		}
	})
}
