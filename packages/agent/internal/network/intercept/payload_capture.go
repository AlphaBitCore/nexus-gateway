package intercept

import "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"

// CaptureRequestBody returns a defensive copy of body when the store has
// StoreRequestBody enabled. The copy is **not** truncated by the agent —
// inline-vs-spill routing happens later at audit-time inside
// agent/cmd/agent/main.go's OnFlowComplete. The intercept layer just
// decides "capture or not".
//
// Mirrors the agent's pre-hook semantics: the agent pipeline runs with
// allowModify=false, so the slice fed to the hook pipeline, the slice
// forwarded upstream, and the slice captured here are all the same bytes.
func CaptureRequestBody(store *payloadcapture.Store, body []byte) []byte {
	if store == nil || len(body) == 0 {
		return nil
	}
	cfg := store.Get()
	if !cfg.StoreRequestBody {
		return nil
	}
	return defensiveCopy(body)
}

// CaptureResponseBody mirrors CaptureRequestBody for the response stage.
// SSE bodies are eligible too — the platform MITM relay buffers SSE
// bytes up to inspectBodyCap (spill.perObjectCap, default 256 MiB) and
// feeds them to this helper exactly like non-streaming responses.
func CaptureResponseBody(store *payloadcapture.Store, body []byte) []byte {
	if store == nil || len(body) == 0 {
		return nil
	}
	cfg := store.Get()
	if !cfg.StoreResponseBody {
		return nil
	}
	return defensiveCopy(body)
}

// defensiveCopy returns a fresh allocation backing the same bytes. The
// copy is important: platform-layer buffers are reused by the MITM read
// loop and must not be aliased from the audit queue.
func defensiveCopy(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
