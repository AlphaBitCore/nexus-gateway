package tlsbump

// Relaying the upstream response back to the client: hop-by-hop scrubbing and the
// header + body copy.
//
// Split out of upstream.go along the direction seam — that file builds the upstream
// transport and sends the request out, this one brings the response back in. The split
// was forced by the file-size ratchet and taken here rather than waived because the two
// halves face opposite ways and share nothing but the hop-by-hop list.

import (
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/responseio"
)

// isHopByHopHeader returns true if the header name is a hop-by-hop header
// that must not be forwarded in proxy responses per RFC 7230 §6.1.
func isHopByHopHeader(name string) bool {
	return hopByHopHeaderSet[http.CanonicalHeaderKey(name)]
}

// copyResponse writes the upstream response back to the client response writer.
// It strips hop-by-hop headers (static list and any names in Connection per
// RFC 7230 §6.1), invokes hook (if non-nil) to let the caller mutate headers
// before they are sent, writes the status code, and streams the body.
//
// The hook parameter uses responseio.HeaderHook so Phase 3 can inject
// x-nexus-* response markers without changing this function's signature.
func copyResponse(w http.ResponseWriter, resp *http.Response, hook responseio.HeaderHook) error {
	defer func() {
		_ = resp.Body.Close()
	}()

	// RFC 7230 §6.1: strip dynamic hop-by-hop headers listed in Connection
	// before deleting Connection itself. Values iterates every Connection line.
	for _, line := range resp.Header.Values("Connection") {
		for _, name := range strings.Split(line, ",") {
			if n := strings.TrimSpace(name); n != "" {
				resp.Header.Del(n)
			}
		}
	}

	// Strip the static set of hop-by-hop headers (includes Connection itself).
	for _, h := range hopByHopHeaders {
		resp.Header.Del(h)
	}

	// Allow the caller to mutate headers (e.g. inject x-nexus-* markers).
	if hook != nil {
		hook(resp)
	}

	// Copy surviving response headers to the client.
	//
	// Assigning the value slice directly, rather than Add-ing each value, skips two
	// costs per header: Add re-canonicalizes the key on every call, and it appends
	// into a fresh slice for each value. The keys here are already canonical —
	// net/http canonicalizes when it parses the response, and every mutation above
	// goes through Set/Del/Add, which canonicalize too.
	//
	// "Already canonical" is asserted rather than assumed, because a future hook
	// writing resp.Header via a raw map index would break it and the failure would
	// be silent: a non-canonical key reaches the wire verbatim instead of being
	// normalized. CanonicalMIMEHeaderKey returns its argument unchanged, without
	// allocating, when the key is already canonical, so the guard costs one call per
	// header and the slow path preserves the old behaviour exactly.
	//
	// The assigned slice is shared with resp.Header rather than copied. That is safe
	// here and only here: resp is closed by this function's defer and dropped, this
	// is the last step before WriteHeader, and nothing mutates either map afterwards.
	dst := w.Header()
	for key, values := range resp.Header {
		// An empty value list must NOT create the key. The Add-per-value loop this
		// replaced never entered its inner loop for such an entry, so the key simply
		// did not appear in the client's header map — and assigning the empty slice
		// would make it appear. The wire bytes are the same either way (a key with no
		// values writes nothing), but anything reading w.Header() afterwards would see
		// a different key set. Only a raw map write can produce this shape, which is
		// exactly why the differential test carries the case rather than an argument
		// that it is unreachable.
		if len(values) == 0 {
			continue
		}
		// The fast path assigns, so it may only be taken when the destination has
		// nothing under that key. Two ways it would otherwise clobber a value that
		// Add would have appended: an earlier stage already set the header on w, or
		// this same loop already processed a non-canonical spelling of it through the
		// slow path below. The second one is ORDER-DEPENDENT — Go randomizes map
		// iteration — so it presents as a flaky wrong answer rather than a
		// reproducible one, which is how the differential test found it.
		if _, taken := dst[key]; !taken && textproto.CanonicalMIMEHeaderKey(key) == key {
			dst[key] = values
			continue
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}

	w.WriteHeader(resp.StatusCode)

	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("copy response body: %w", err)
	}

	// Flush if the writer supports it (important for SSE / streaming).
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	return nil
}
