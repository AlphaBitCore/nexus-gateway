package codec

// videos_test.go — business-behavior tests for the Veo video codec (e88-s6
// §3b): id round-trip + hostile-decode containment, allow-list-only encode
// with the lossy size map, and the LRO → canonical lifecycle mapping.

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestVeoJobID_RoundTrip(t *testing.T) {
	op := "models/veo-3.1-generate-preview/operations/abc-123_XYZ"
	id := VeoJobID(op)
	if !strings.HasPrefix(id, "veo_") || strings.ContainsAny(id, "/+=") {
		t.Fatalf("id %q must be veo_-prefixed and URL-safe single-segment", id)
	}
	if !IsVeoJobID(id) {
		t.Fatalf("IsVeoJobID(%q) = false", id)
	}
	back, err := VeoOperationName(id)
	if err != nil || back != op {
		t.Fatalf("round-trip = (%q, %v), want the original operation name", back, err)
	}
}

// A hostile provider mints the operation name at submit — every decode must
// contain it: traversal segments, metacharacters, absolute paths, empty, and
// non-base64 garbage all refuse.
func TestVeoOperationName_HostileDecodesRefused(t *testing.T) {
	enc := func(s string) string { return "veo_" + base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, bad := range []string{
		enc("../../v1beta/admin"),
		enc("models/../secrets"),
		enc("/etc/passwd"),
		enc("models//operations"),
		enc("a?b=c"),
		enc("a#frag"),
		enc(`a\b`),
		enc("a%2Fb"),
		enc("ops/" + string(rune(0x202e)) + "evil"), // unicode control
		enc(""),
		"veo_!!!not-base64",
		"video_abc", // not a veo id at all
	} {
		if _, err := VeoOperationName(bad); err == nil {
			t.Errorf("VeoOperationName(%q) accepted a hostile value", bad)
		}
	}
}

func TestEncodeVeoSubmit_AllowListOnly(t *testing.T) {
	_, _, err := EncodeVeoSubmit(VeoSubmitParams{
		Prompt:          "a cat",
		ExtraFieldNames: []string{"systemInstruction"},
	})
	if err == nil || !strings.Contains(err.Error(), "systemInstruction") {
		t.Fatalf("extra field must 400 naming the field; err = %v", err)
	}
}

func TestEncodeVeoSubmit_SizeMapAndCoercions(t *testing.T) {
	body, coercions, err := EncodeVeoSubmit(VeoSubmitParams{
		Prompt: "a cat", SecondsInt: 8, Size: "1792x1024",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	params := m["parameters"].(map[string]any)
	if params["aspectRatio"] != "16:9" || params["resolution"] != "1080p" || params["durationSeconds"] != float64(8) {
		t.Errorf("parameters = %v, want 16:9/1080p/8", params)
	}
	inst := m["instances"].([]any)[0].(map[string]any)
	if inst["prompt"] != "a cat" {
		t.Errorf("prompt = %v", inst["prompt"])
	}
	if len(coercions) != 1 || !strings.Contains(coercions[0], "size:1792x1024") {
		t.Errorf("coercions = %v, want the lossy size marker", coercions)
	}

	// Unmapped size → 400, never a guess.
	if _, _, err := EncodeVeoSubmit(VeoSubmitParams{Prompt: "x", Size: "640x480"}); err == nil {
		t.Error("unmapped size must refuse")
	}
}

func TestEncodeVeoSubmit_InputRefInlinesBase64(t *testing.T) {
	raw := []byte{0x89, 'P', 'N', 'G'}
	body, _, err := EncodeVeoSubmit(VeoSubmitParams{
		Prompt: "animate this", InputRefBytes: raw, InputRefMime: "image/png",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var m struct {
		Instances []struct {
			Image *struct {
				InlineData struct {
					MimeType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
			} `json:"image"`
		} `json:"instances"`
		Parameters any `json:"parameters"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	img := m.Instances[0].Image
	if img == nil || img.InlineData.MimeType != "image/png" ||
		img.InlineData.Data != base64.StdEncoding.EncodeToString(raw) {
		t.Errorf("inlineData mismatch: %+v", img)
	}
	if m.Parameters != nil {
		t.Errorf("parameters must be omitted when nothing is set (got %v)", m.Parameters)
	}
}

func TestDecodeVeoOperation_LifecycleMapping(t *testing.T) {
	created := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	id := VeoJobID("models/veo-3.1/operations/op1")

	// done:false → in_progress (never queued — the LRO has no intermediate
	// states), progress 0, seconds echoed, expires_at synthesized
	// createdAt+48h, completed_at absent.
	v, err := DecodeVeoOperation([]byte(`{"name":"models/veo-3.1/operations/op1","done":false}`), id, "veo-3.1", 8, created)
	if err != nil || v.Status != "in_progress" {
		t.Fatalf("running = (%q, %v), want in_progress", v.Status, err)
	}
	if !v.ExpiresAt.Equal(created.Add(48 * time.Hour)) {
		t.Errorf("expires_at = %v, want createdAt+48h", v.ExpiresAt)
	}
	var obj map[string]any
	_ = json.Unmarshal(v.ClientBody, &obj)
	if obj["id"] != id || obj["object"] != "video" || obj["status"] != "in_progress" ||
		obj["model"] != "veo-3.1" || obj["progress"] != float64(0) || obj["seconds"] != float64(8) ||
		obj["expires_at"] != float64(created.Add(48*time.Hour).Unix()) {
		t.Errorf("canonical object = %v", obj)
	}
	if _, hasCompleted := obj["completed_at"]; hasCompleted {
		t.Errorf("completed_at must be absent while running (got %v)", obj["completed_at"])
	}

	// done + error → failed with the provider detail surfaced.
	v, err = DecodeVeoOperation([]byte(`{"done":true,"error":{"code":3,"message":"safety block"}}`), id, "veo-3.1", 8, created)
	if err != nil || v.Status != "failed" {
		t.Fatalf("failed = (%q, %v)", v.Status, err)
	}
	_ = json.Unmarshal(v.ClientBody, &obj)
	errObj := obj["error"].(map[string]any)
	if errObj["code"] != "veo_3" || errObj["message"] != "safety block" {
		t.Errorf("error detail = %v", errObj)
	}

	// done + response → completed with the artifact URI extracted, progress 100.
	lro := `{"done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":"https://generativelanguage.googleapis.com/v1beta/files/x:download"}}]}}}`
	v, err = DecodeVeoOperation([]byte(lro), id, "veo-3.1", 8, created)
	if err != nil || v.Status != "completed" || !strings.HasPrefix(v.VideoURI, "https://generativelanguage") {
		t.Fatalf("completed = (%q, %q, %v)", v.Status, v.VideoURI, err)
	}
	_ = json.Unmarshal(v.ClientBody, &obj)
	if obj["status"] != "completed" || obj["progress"] != float64(100) {
		t.Errorf("completed object = %v", obj)
	}

	// done + response with ZERO samples (all safety-filtered) → FAILED, no
	// URI — never a completed job that dead-ends the download and bills for
	// an undelivered render.
	rai := `{"done":true,"response":{"generateVideoResponse":{"generatedSamples":[]}}}`
	v, err = DecodeVeoOperation([]byte(rai), id, "veo-3.1", 8, created)
	if err != nil || v.Status != "failed" || v.VideoURI != "" {
		t.Fatalf("rai-filtered = (%q, %q, %v), want failed with no URI", v.Status, v.VideoURI, err)
	}
	_ = json.Unmarshal(v.ClientBody, &obj)
	if obj["error"].(map[string]any)["code"] != "veo_no_output" {
		t.Errorf("rai-filtered error = %v, want veo_no_output", obj["error"])
	}

	// Garbage → error, never a zero-value view a handler would relay.
	if _, err := DecodeVeoOperation([]byte("not json"), id, "m", 8, created); err == nil {
		t.Error("garbage LRO must refuse")
	}
}

func TestVeoOperationNameFromSubmit(t *testing.T) {
	op, err := VeoOperationNameFromSubmit([]byte(`{"name":"models/veo-3.1/operations/xyz"}`))
	if err != nil || op != "models/veo-3.1/operations/xyz" {
		t.Fatalf("= (%q, %v)", op, err)
	}
	if _, err := VeoOperationNameFromSubmit([]byte(`{}`)); err == nil {
		t.Error("missing name must refuse (the job would be uncorrelatable)")
	}
}
