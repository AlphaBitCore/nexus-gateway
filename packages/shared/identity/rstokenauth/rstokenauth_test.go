package rstokenauth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/rstokenauth"
)

func okHandler(c echo.Context) error { return c.String(http.StatusOK, "ok") }

func TestMiddleware_ValidToken_Allows(t *testing.T) {
	e := echo.New()
	e.Use(rstokenauth.Middleware("secret-123"))
	e.GET("/ok", okHandler)

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	req.Header.Set(rstokenauth.Header, "secret-123")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "ok")
	}
}

func TestMiddleware_InvalidToken_401(t *testing.T) {
	e := echo.New()
	e.Use(rstokenauth.Middleware("secret-123"))
	e.GET("/ok", okHandler)

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	req.Header.Set(rstokenauth.Header, "wrong")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error envelope; got %s", rec.Body.String())
	}
	if errObj["code"] != "RS_TOKEN_INVALID" {
		t.Errorf("code = %v, want RS_TOKEN_INVALID", errObj["code"])
	}
}

func TestMiddleware_MissingToken_401(t *testing.T) {
	e := echo.New()
	e.Use(rstokenauth.Middleware("secret-123"))
	e.GET("/ok", okHandler)

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error envelope; got %s", rec.Body.String())
	}
	if errObj["code"] != "RS_TOKEN_REQUIRED" {
		t.Errorf("code = %v, want RS_TOKEN_REQUIRED", errObj["code"])
	}
}

func TestMiddleware_EmptySecret_503(t *testing.T) {
	e := echo.New()
	e.Use(rstokenauth.Middleware(""))
	e.GET("/ok", okHandler)

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	req.Header.Set(rstokenauth.Header, "anything")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty secret should 503; got %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error envelope; got %s", rec.Body.String())
	}
	if errObj["code"] != "RS_TOKEN_NOT_CONFIGURED" {
		t.Errorf("code = %v, want RS_TOKEN_NOT_CONFIGURED", errObj["code"])
	}
}

func TestMiddlewareHTTP_ValidToken_Allows(t *testing.T) {
	handler := rstokenauth.MiddlewareHTTP("x")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(rstokenauth.Header, "x")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("valid token: got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestMiddlewareHTTP_InvalidToken_401(t *testing.T) {
	handler := rstokenauth.MiddlewareHTTP("x")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(rstokenauth.Header, "wrong")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token: got %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "RS_TOKEN_INVALID" {
		t.Errorf("code = %v, want RS_TOKEN_INVALID", errObj["code"])
	}
}

func TestMiddlewareHTTP_EmptySecret_503(t *testing.T) {
	handler := rstokenauth.MiddlewareHTTP("")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(rstokenauth.Header, "anything")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty secret: got %d", rec.Code)
	}
}

func TestMiddlewareHTTP_MissingToken_401(t *testing.T) {
	handler := rstokenauth.MiddlewareHTTP("x")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d", rec.Code)
	}
}
