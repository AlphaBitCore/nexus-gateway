package proxy

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// buildSTTMultipartWithPrompt is buildSTTMultipart plus a prompt form field —
// the request-side text leaf the compliance scan reads.
func buildSTTMultipartWithPrompt(t *testing.T, model, prompt string, audio []byte) ([]byte, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	if err := w.WriteField("model", model); err != nil {
		t.Fatalf("write model: %v", err)
	}
	if prompt != "" {
		if err := w.WriteField("prompt", prompt); err != nil {
			t.Fatalf("write prompt: %v", err)
		}
	}
	fw, err := w.CreateFormFile("file", "audio.mp3")
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := fw.Write(audio); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

// sttPromptCapturingUpstream returns a stub STT upstream that parses the
// forwarded multipart and records the prompt field it received.
func sttPromptCapturingUpstream(t *testing.T, respBody string) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var gotPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err == nil {
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				p, perr := mr.NextPart()
				if perr != nil {
					break
				}
				if p.FormName() == "prompt" {
					b, _ := io.ReadAll(p)
					mu.Lock()
					gotPrompt = string(b)
					mu.Unlock()
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, func() string { mu.Lock(); defer mu.Unlock(); return gotPrompt }
}

// A PII-laden prompt under a redact hook is REDACTED AND FORWARDED: unlike the
// video wire, the STT ReEmit can carry the sanitized value, so the upstream
// must receive the redacted prompt (never the raw email) and the request
// still succeeds with coverage prompt-only.
func TestServeSTT_PromptRedactReEmit(t *testing.T) {
	srv, gotPrompt := sttPromptCapturingUpstream(t, `{"text":"ok","usage":{"seconds":2}}`)
	deps, prod := sttDeps(t, srv.URL)
	deps.HookConfigCache = newPiiRedactHookCache(t)
	h := NewHandler(deps)

	body, ct := buildSTTMultipartWithPrompt(t, "whisper-1", "transcribe the call with cat@example.com", []byte("RIFFxxxx"))
	rr := doSTT(h.ServeSTT(sttIngress()), "/v1/audio/transcriptions", ct, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("= %d %s, want 200 (redact forwards, not fail-closed)", rr.Code, rr.Body.String())
	}
	up := gotPrompt()
	if strings.Contains(up, "cat@example.com") {
		t.Fatalf("unredacted prompt leaked upstream: %q", up)
	}
	if !strings.Contains(up, "[REDACTED_EMAIL]") {
		t.Fatalf("upstream prompt not redacted: %q", up)
	}
	msg := lastAudit(t, deps, prod)
	if msg.ComplianceCoverage != "prompt-only" {
		t.Errorf("coverage = %q, want prompt-only", msg.ComplianceCoverage)
	}
}

// A benign prompt under a content hook passes untouched with coverage
// prompt-only; the upstream receives the original text.
func TestServeSTT_BenignPromptScanned(t *testing.T) {
	srv, gotPrompt := sttPromptCapturingUpstream(t, `{"text":"ok","usage":{"seconds":2}}`)
	deps, prod := sttDeps(t, srv.URL)
	deps.HookConfigCache = newPiiRedactHookCache(t)
	h := NewHandler(deps)

	body, ct := buildSTTMultipartWithPrompt(t, "whisper-1", "medical vocabulary hints", []byte("RIFFxxxx"))
	rr := doSTT(h.ServeSTT(sttIngress()), "/v1/audio/transcriptions", ct, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("= %d %s, want 200", rr.Code, rr.Body.String())
	}
	if up := gotPrompt(); up != "medical vocabulary hints" {
		t.Fatalf("benign prompt altered: %q", up)
	}
	msg := lastAudit(t, deps, prod)
	if msg.ComplianceCoverage != "prompt-only" {
		t.Errorf("coverage = %q, want prompt-only", msg.ComplianceCoverage)
	}
}

// A hard-rejecting hook blocks the request with 403 before any upstream call.
func TestServeSTT_PromptBlocked(t *testing.T) {
	deps, _ := sttDeps(t, "http://unused.invalid")
	deps.HookConfigCache = newRejectingHookCache(t, nil)
	h := NewHandler(deps)

	body, ct := buildSTTMultipartWithPrompt(t, "whisper-1", "anything", []byte("RIFFxxxx"))
	rr := doSTT(h.ServeSTT(sttIngress()), "/v1/audio/transcriptions", ct, body)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("= %d %s, want 403", rr.Code, rr.Body.String())
	}
}

// No prompt field: nothing to scan, coverage stays none, request flows.
func TestServeSTT_NoPromptCoverageNone(t *testing.T) {
	srv, _ := sttPromptCapturingUpstream(t, `{"text":"ok","usage":{"seconds":2}}`)
	deps, prod := sttDeps(t, srv.URL)
	deps.HookConfigCache = newPiiRedactHookCache(t)
	h := NewHandler(deps)

	body, ct := buildSTTMultipart(t, "whisper-1", "", []byte("RIFFxxxx"))
	rr := doSTT(h.ServeSTT(sttIngress()), "/v1/audio/transcriptions", ct, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("= %d %s, want 200", rr.Code, rr.Body.String())
	}
	msg := lastAudit(t, deps, prod)
	if msg.ComplianceCoverage != "none" {
		t.Errorf("coverage = %q, want none (no prompt to scan)", msg.ComplianceCoverage)
	}
}
