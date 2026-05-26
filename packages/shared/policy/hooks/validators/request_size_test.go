package validators

import (
	"context"
	"strings"
	"testing"
)

func newRequestSizeHook(t *testing.T, cfg map[string]any) Hook {
	t.Helper()
	h, err := NewRequestSizeValidator(&HookConfig{
		ID:               "rsv-1",
		ImplementationID: "request-size-validator",
		Name:             "test-request-size",
		Stage:            "request",
		Config:           cfg,
	})
	if err != nil {
		t.Fatalf("NewRequestSizeValidator: %v", err)
	}
	return h
}

// --- Factory error paths ----------------------------------------------------

func TestRequestSize_Factory_MissingMaxSizeRejected(t *testing.T) {
	_, err := NewRequestSizeValidator(&HookConfig{Config: map[string]any{}})
	if err == nil {
		t.Fatal("missing maxSizeBytes should error")
	}
	if !strings.Contains(err.Error(), "maxSizeBytes") {
		t.Errorf("error should mention field name: %v", err)
	}
}

func TestRequestSize_Factory_NegativeMaxSizeRejected(t *testing.T) {
	_, err := NewRequestSizeValidator(&HookConfig{Config: map[string]any{
		"maxSizeBytes": float64(-1),
	}})
	if err == nil {
		t.Fatal("negative maxSizeBytes should error")
	}
}

func TestRequestSize_Factory_ZeroMaxSizeRejected(t *testing.T) {
	_, err := NewRequestSizeValidator(&HookConfig{Config: map[string]any{
		"maxSizeBytes": float64(0),
	}})
	if err == nil {
		t.Fatal("zero maxSizeBytes should error")
	}
}

func TestRequestSize_Factory_StringMaxSizeRejected(t *testing.T) {
	_, err := NewRequestSizeValidator(&HookConfig{Config: map[string]any{
		"maxSizeBytes": "10000",
	}})
	if err == nil {
		t.Fatal("non-numeric maxSizeBytes should error")
	}
}

func TestRequestSize_Factory_ExcludeContentTypesNotArrayRejected(t *testing.T) {
	_, err := NewRequestSizeValidator(&HookConfig{Config: map[string]any{
		"maxSizeBytes":        float64(1024),
		"excludeContentTypes": "multipart/form-data",
	}})
	if err == nil {
		t.Fatal("non-array excludeContentTypes should error")
	}
	if !strings.Contains(err.Error(), "excludeContentTypes") {
		t.Errorf("error should mention field name: %v", err)
	}
}

func TestRequestSize_Factory_ExcludeContentTypesNormalizedLowercase(t *testing.T) {
	// "MULTIPART/FORM-DATA" must normalize so the matching is case-insensitive.
	h := newRequestSizeHook(t, map[string]any{
		"maxSizeBytes":        float64(100),
		"excludeContentTypes": []any{"MULTIPART/FORM-DATA"},
	})
	// A body that would exceed limit must be approved because of the exclude.
	res, _ := h.Execute(context.Background(), &HookInput{
		BodySize:    9999,
		ContentType: "multipart/form-data",
	})
	if res.Decision != Approve {
		t.Errorf("normalized exclude should approve; got %s", res.Decision)
	}
}

func TestRequestSize_Factory_ExcludeContentTypesSkipsEmptyEntries(t *testing.T) {
	// Empty string entries in excludeContentTypes must be silently skipped
	// (operator typo); the factory itself must not error.
	h, err := NewRequestSizeValidator(&HookConfig{
		ID: "rsv",
		Config: map[string]any{
			"maxSizeBytes":        float64(100),
			"excludeContentTypes": []any{"", "application/json", 42, ""},
		},
	})
	if err != nil {
		t.Fatalf("empty/non-string excludes should be silently skipped: %v", err)
	}
	rsv := h.(*RequestSizeValidator)
	if _, ok := rsv.excludeContentTypes["application/json"]; !ok {
		t.Errorf("application/json should be in the exclude map: %+v", rsv.excludeContentTypes)
	}
	if _, ok := rsv.excludeContentTypes[""]; ok {
		t.Errorf("empty string should not be in the exclude map")
	}
}

// --- Execute -----------------------------------------------------------------

func TestRequestSize_Execute_UnderLimitApproves(t *testing.T) {
	h := newRequestSizeHook(t, map[string]any{"maxSizeBytes": float64(1024)})
	res, err := h.Execute(context.Background(), &HookInput{
		BodySize: 100, ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Approve {
		t.Errorf("under-limit: got %s want Approve", res.Decision)
	}
}

func TestRequestSize_Execute_OverLimitRejects(t *testing.T) {
	h := newRequestSizeHook(t, map[string]any{"maxSizeBytes": float64(1024)})
	res, err := h.Execute(context.Background(), &HookInput{
		BodySize: 4096, ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != RejectHard {
		t.Errorf("over-limit: got %s want RejectHard", res.Decision)
	}
	if res.ReasonCode != "REQUEST_TOO_LARGE" {
		t.Errorf("reasonCode: got %q want REQUEST_TOO_LARGE", res.ReasonCode)
	}
	if !strings.Contains(res.Reason, "4096") || !strings.Contains(res.Reason, "1024") {
		t.Errorf("reason should mention both actual and limit: %q", res.Reason)
	}
}

func TestRequestSize_Execute_ExactLimitApproves(t *testing.T) {
	// Boundary: BodySize == maxSizeBytes must approve (strict >, not >=).
	h := newRequestSizeHook(t, map[string]any{"maxSizeBytes": float64(1024)})
	res, _ := h.Execute(context.Background(), &HookInput{BodySize: 1024})
	if res.Decision != Approve {
		t.Errorf("exact-limit boundary: got %s want Approve", res.Decision)
	}
}

func TestRequestSize_Execute_ExcludedContentTypeBypassesCheck(t *testing.T) {
	// Excluded content type → approve regardless of size.
	h := newRequestSizeHook(t, map[string]any{
		"maxSizeBytes":        float64(100),
		"excludeContentTypes": []any{"multipart/form-data"},
	})
	res, _ := h.Execute(context.Background(), &HookInput{
		BodySize: 50_000_000, ContentType: "multipart/form-data",
	})
	if res.Decision != Approve {
		t.Errorf("excluded ct: got %s want Approve (size check should be skipped)", res.Decision)
	}
}

func TestRequestSize_Execute_ContentTypeWithParametersMatchesBase(t *testing.T) {
	// Real-world headers include charset/boundary: the base type before ';'
	// is what the exclude list should match against.
	h := newRequestSizeHook(t, map[string]any{
		"maxSizeBytes":        float64(100),
		"excludeContentTypes": []any{"application/json"},
	})
	res, _ := h.Execute(context.Background(), &HookInput{
		BodySize: 9999, ContentType: "application/json; charset=utf-8",
	})
	if res.Decision != Approve {
		t.Errorf("parameterized ct: got %s want Approve (base match)", res.Decision)
	}
}

func TestRequestSize_Execute_NonExcludedContentTypeStillChecked(t *testing.T) {
	// Non-excluded content type goes through normal check.
	h := newRequestSizeHook(t, map[string]any{
		"maxSizeBytes":        float64(100),
		"excludeContentTypes": []any{"multipart/form-data"},
	})
	res, _ := h.Execute(context.Background(), &HookInput{
		BodySize: 200, ContentType: "application/json",
	})
	if res.Decision != RejectHard {
		t.Errorf("non-excluded over-limit: got %s want RejectHard", res.Decision)
	}
}

func TestRequestSize_Execute_HookMetadataPropagated(t *testing.T) {
	h := newRequestSizeHook(t, map[string]any{"maxSizeBytes": float64(1024)})
	res, _ := h.Execute(context.Background(), &HookInput{BodySize: 10})
	if res.HookID != "rsv-1" {
		t.Errorf("HookID: %q", res.HookID)
	}
	if res.ImplementationID != "request-size-validator" {
		t.Errorf("ImplementationID: %q", res.ImplementationID)
	}
	if res.HookName != "test-request-size" {
		t.Errorf("HookName: %q", res.HookName)
	}
	if res.LatencyMs < 0 {
		t.Errorf("LatencyMs negative: %d", res.LatencyMs)
	}
}

func TestRequestSize_Execute_IntMaxSizeAccepted(t *testing.T) {
	// toInt64 accepts int / int64 / float64. Verify the int path works
	// (JSON deserialisation typically yields float64; raw Go construction
	// may use int).
	h, err := NewRequestSizeValidator(&HookConfig{
		ID: "rsv-int", Config: map[string]any{"maxSizeBytes": int(2048)},
	})
	if err != nil {
		t.Fatalf("int maxSizeBytes: %v", err)
	}
	res, _ := h.Execute(context.Background(), &HookInput{BodySize: 5000})
	if res.Decision != RejectHard {
		t.Errorf("int path: got %s want RejectHard", res.Decision)
	}
}

func TestRequestSize_Execute_Int64MaxSizeAccepted(t *testing.T) {
	h, err := NewRequestSizeValidator(&HookConfig{
		ID: "rsv-i64", Config: map[string]any{"maxSizeBytes": int64(4096)},
	})
	if err != nil {
		t.Fatalf("int64 maxSizeBytes: %v", err)
	}
	res, _ := h.Execute(context.Background(), &HookInput{BodySize: 100})
	if res.Decision != Approve {
		t.Errorf("int64 path: got %s want Approve", res.Decision)
	}
}
