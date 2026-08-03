package proxy

// video_content_handler.go — ServeVideoContent, the streamed artifact
// download for GET /v1/videos/{id}/content (e88-s6 T7, D-V5/D-V5a). The
// gateway relays the provider's authenticated download endpoint through a
// fingerprint tee: no artifact store, no gateway-minted URLs — custody stays
// with the provider, the gateway records WHAT passed through (sha256 / size /
// mime onto the download row's artifact_refs).
//
// Relay hygiene (D-V5): the artifact bytes are provider-controlled, so the
// relay enforces a Content-Type allowlist (anything else → 502 — a
// compromised provider must not serve active content through the
// authenticated gateway origin), nosniff + Content-Disposition: attachment,
// and a hard 1 GiB ceiling. The ceiling NEVER truncates silently: a declared
// oversize body is a clean 502 before any byte is written; a mid-stream
// overflow aborts the connection (http.ErrAbortHandler) so the client sees a
// transport failure, not a plausible short file.
//
// The generated video is NOT content-scanned (R-1 stands, owner+legal-gated);
// compliance_coverage=none on the download row is the honest record, and this
// tee is R-1's named remediation mount point.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store/asyncjob"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/generativecaps"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provdispatch "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/dispatch"
	geminicodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/codec"
	sharedstreaming "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// videoContentMaxBytes is the hard artifact-relay ceiling (D-V5): a defensive
// bound far above any real render, so a misbehaving provider cannot stream
// unbounded bytes through the gateway. A var only so tests can drive the
// mid-stream overflow arm without streaming a real gibibyte.
var videoContentMaxBytes int64 = 1 << 30 // 1 GiB

// videoContentTypeAllowed pins the artifact Content-Type allowlist (D-V5):
// the video itself plus the thumbnail/spritesheet image variants. The base
// media type is matched after mime.ParseMediaType, so parameters
// (`; codecs=…`) cannot dodge the check.
func videoContentTypeAllowed(baseType string) bool {
	switch baseType {
	case "video/mp4", "image/jpeg", "image/png", "image/webp":
		return true
	}
	return false
}

// errVideoContentOverflow is the ceilingReader's sentinel: the upstream body
// crossed videoContentMaxBytes mid-stream.
var errVideoContentOverflow = errors.New("video content exceeds the relay ceiling")

// ceilingReader errors (rather than EOF-truncates) once bytes beyond the
// budget exist — the distinction between "the artifact ended" and "the
// artifact is larger than the gateway will relay" must be an explicit
// failure, never a silently shortened file. An artifact of EXACTLY the
// budget must relay cleanly: when the budget is spent, a one-byte probe of
// the wrapped reader distinguishes a trailing EOF (clean end — io.Reader's
// contract permits either coalesced (n, io.EOF) or a split final (0, io.EOF),
// so the probe must not assume the coalesced shape) from real further bytes
// (overflow).
type ceilingReader struct {
	r         io.Reader
	remaining int64
}

func (c *ceilingReader) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		var probe [1]byte
		for {
			n, err := c.r.Read(probe[:])
			if n > 0 {
				return 0, errVideoContentOverflow
			}
			if err != nil {
				return 0, err // io.EOF = ended exactly at the ceiling — clean
			}
			// (0, nil) is a legal no-op read; try again.
		}
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	return n, err
}

// fingerprintTee hashes and counts everything read through it — the
// artifact_refs record of what the client actually received.
type fingerprintTee struct {
	r    io.Reader
	h    hash.Hash
	size int64
}

func newFingerprintTee(r io.Reader) *fingerprintTee {
	return &fingerprintTee{r: r, h: sha256.New()}
}

func (t *fingerprintTee) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		_, _ = t.h.Write(p[:n])
		t.size += int64(n)
	}
	return n, err
}

// videoContentDisposition builds the download disposition. A filename rides
// along only when the job id is filename-safe (the id is provider-controlled;
// a quote or control character must never reach a header value) — otherwise
// the bare attachment still forces download, just without a suggested name.
func videoContentDisposition(jobID, baseType string) string {
	ext := map[string]string{
		"video/mp4":  "mp4",
		"image/jpeg": "jpg",
		"image/png":  "png",
		"image/webp": "webp",
	}[baseType]
	for _, r := range jobID {
		safe := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-'
		if !safe {
			return "attachment"
		}
	}
	if jobID == "" || ext == "" {
		return "attachment"
	}
	return fmt.Sprintf("attachment; filename=%q", jobID+"."+ext)
}

// ServeVideoContent returns the HTTP handler for GET /v1/videos/{id}/content:
// job-row authz (the same owned lookup + 404 non-disclosure + 410-expired as
// poll/delete) → pinned-credential resolve → streamed relay through the
// fingerprint tee. Only the `variant` query parameter is forwarded
// (video|thumbnail|spritesheet are the provider's values — it owns the enum
// and its 400 relays; nothing else from the client's query string passes).
func (h *Handler) ServeVideoContent(in Ingress) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.gate != nil {
			if !h.gate.acquire() {
				writeOverloaded(w, in.BodyFormat)
				return
			}
			defer h.gate.release()
		}

		start := time.Now().UTC()
		rec := newVideoFollowRecord(r, in, start)
		// The generated artifact is not content-scanned (R-1) — none is the
		// honest coverage for every outcome of this route.
		rec.ComplianceCoverage = "none"
		var releaseCap func()
		defer func() {
			if releaseCap != nil {
				releaseCap()
			}
			if h.deps.AuditWriter != nil {
				h.finalize(rec, start)
			}
		}()

		vkMeta, job, ok := h.videoFollowJob(w, r, rec)
		if !ok {
			return
		}

		// Per-(VK, video) generative-caps concurrency: unlike the quick
		// 1 MiB poll/delete relays, a content transfer can hold its slot for
		// a GiB-long stream — without a per-VK bound one key could pin many
		// global admission slots at once (the review-named fairness gap).
		// Same registry row the submit path uses; no new knob.
		caps, generative := generativecaps.Lookup(typology.EndpointKindVideoGeneration)
		if generative && caps.MaxConcurrentPerVK > 0 && h.genConcurrency != nil {
			if !h.genConcurrency.Acquire(typology.EndpointKindVideoGeneration, vkMeta.ID, caps.MaxConcurrentPerVK) {
				generativeCapShedTotal.WithLabelValues(string(typology.EndpointKindVideoGeneration)).Inc()
				w.Header().Set("Retry-After", "1")
				h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "GENERATIVE_CONCURRENCY_LIMIT",
					"too many concurrent video transfers for this key",
					"Reduce concurrent downloads or retry shortly")
				return
			}
			vkID := vkMeta.ID
			releaseCap = func() { h.genConcurrency.Release(typology.EndpointKindVideoGeneration, vkID) }
		}

		if geminicodec.IsVeoJobID(job.ID) {
			h.serveVeoContent(w, r, rec, job)
			return
		}

		var rawQuery string
		if v := r.URL.Query().Get("variant"); v != "" {
			rawQuery = url.Values{"variant": {v}}.Encode()
		}
		upReq, callTarget, ok := h.videoFollowRequest(w, r, rec, job, http.MethodGet, "/content", rawQuery)
		if !ok {
			return
		}
		resp, doErr := h.deps.UpstreamClient.Do(upReq)
		if doErr != nil {
			h.writeDetailedErr(w, rec, http.StatusBadGateway, "UPSTREAM_ERROR",
				"upstream provider request failed", "Retry; the provider may be unavailable")
			return
		}
		defer func() { _ = resp.Body.Close() }()
		rec.StatusCode = resp.StatusCode

		// Provider errors (expired artifact, unknown variant, …) relay
		// verbatim through the buffered small-body path — same posture as
		// poll.
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			respBody, _ := readResponseBounded(resp, videoFollowMaxResponseBytes)
			h.relayVideoUpstream(w, callTarget.Format, resp.Header, resp.StatusCode, respBody)
			return
		}
		h.streamVideoArtifact(w, r, rec, job, resp, callTarget.Format)
	}
}

// streamVideoArtifact relays one 2xx artifact response through the D-V5
// hygiene set: CT allowlist (before any byte), declared-oversize 502,
// nosniff + attachment, the 1 GiB ceiling (mid-stream overflow → connection
// abort), and the complete-relay-only fingerprint stamp. Shared by the
// OpenAI content-endpoint leg and the Veo URI-fetch leg (D-V5b) — both must
// carry identical hygiene.
func (h *Handler) streamVideoArtifact(w http.ResponseWriter, r *http.Request, rec *audit.Record,
	job asyncjob.Job, resp *http.Response, format provcore.Format) {
	// Content-Type allowlist (D-V5): a 2xx artifact outside the closed
	// set is refused BEFORE any byte reaches the client.
	ct := resp.Header.Get("Content-Type")
	baseType, _, ctErr := mime.ParseMediaType(ct)
	if ctErr != nil || !videoContentTypeAllowed(baseType) {
		h.deps.Logger.Warn("video content: provider served a non-allowlisted content type; refusing the relay",
			slog.String("job", job.ID), slog.String("contentType", ct))
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "VIDEO_CONTENT_TYPE_REJECTED",
			"the provider served an unexpected artifact content type",
			"Retry; if it persists the provider's content endpoint may have changed")
		return
	}

	// A declared oversize artifact fails clean while a 502 is still
	// possible (nothing written yet).
	if resp.ContentLength > videoContentMaxBytes {
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "VIDEO_CONTENT_TOO_LARGE",
			"the artifact exceeds the gateway's relay ceiling",
			"Ask an operator to retrieve the artifact from the provider account")
		return
	}

	// Stream: allowlisted headers + the validated Content-Type +
	// Content-Length when declared (lets the client detect an aborted
	// transfer) + nosniff/attachment (D-V5).
	filtered := provdispatch.FilterResponseHeaders(h.deps.Allowlist, format, resp.Header, false)
	for k, vs := range filtered {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Type", ct)
	if resp.ContentLength > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", resp.ContentLength))
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", videoContentDisposition(job.ID, baseType))
	w.WriteHeader(resp.StatusCode)

	tee := newFingerprintTee(&ceilingReader{r: resp.Body, remaining: videoContentMaxBytes})
	relayErr := sharedstreaming.Passthrough(r.Context(), tee, w)
	if errors.Is(relayErr, errVideoContentOverflow) {
		// Mid-stream overflow: the status line is committed, so the only
		// honest signal left is a transport abort — the client must see a
		// failed download, never a plausible short file. The audit row
		// keeps the committed 200 but carries the abort classification
		// (a bare 200-with-empty-refs could not distinguish overflow from
		// a client disconnect).
		rec.ErrorCode = "VIDEO_CONTENT_RELAY_ABORTED"
		rec.ErrorReason = "artifact exceeded the relay ceiling mid-stream; connection aborted"
		h.deps.Logger.Warn("video content: artifact crossed the relay ceiling mid-stream; aborting the connection",
			slog.String("job", job.ID), slog.Int64("ceiling", videoContentMaxBytes))
		panic(http.ErrAbortHandler)
	}
	if relayErr != nil {
		// Upstream read error or client disconnect mid-stream — the copy
		// already stopped; nothing more can be written. Record what
		// happened for the audit trail.
		rec.ErrorCode = "VIDEO_CONTENT_RELAY_INCOMPLETE"
		rec.ErrorReason = relayErr.Error()
		h.deps.Logger.Warn("video content: relay ended early",
			slog.String("job", job.ID), slog.String("error", relayErr.Error()))
		return
	}

	// Complete relay: fingerprint what the client received (D-V5) —
	// reference-only, the same artifact_refs shape as the input-side
	// fingerprints.
	rec.ArtifactRefs = fmt.Sprintf(`[{"sha256":%q,"sizeBytes":%d,"mime":%q}]`,
		hex.EncodeToString(tee.h.Sum(nil)), tee.size, baseType)
}
