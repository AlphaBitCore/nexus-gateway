package codec

// videos.go — the Veo cross-shape video codec (e88-s6 §3b): canonical =
// OpenAI /v1/videos, wire = Gemini `:predictLongRunning` + long-running
// operations. Follows the image-codec discipline: leg-scoped ALLOW-LIST-ONLY
// (an unknown canonical field is a 400, never forwarded — a forwarded extra
// would ride under the prompt scanner), caller-visible coercion markers, and
// structured build (typed structs + json.Marshal, never string interpolation).
//
// The canonical job id for this leg is `veo_` + base64url(operation name):
// Veo's operation name contains slashes ("models/x/operations/y"), which can
// neither be a single-segment `{id}` path parameter nor a safe URL component
// — the encoding IS the id mapping (deterministic, reversible, no id table),
// and the prefix lets the handler pick the leg without a DB hit.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// veoIDPrefix marks a canonical job id as belonging to the Veo leg.
const veoIDPrefix = "veo_"

// VeoJobID encodes a Veo operation name into the canonical job id.
func VeoJobID(operationName string) string {
	return veoIDPrefix + base64.RawURLEncoding.EncodeToString([]byte(operationName))
}

// IsVeoJobID reports whether a canonical job id belongs to the Veo leg.
func IsVeoJobID(id string) bool {
	return strings.HasPrefix(id, veoIDPrefix)
}

// VeoOperationName decodes the canonical job id back to the operation name,
// validating the decoded value as a safe relative URL path: non-empty
// slash-separated segments, none empty or a dot segment, no query/fragment
// metacharacters. The id was minted from a provider response at submit, so a
// hostile provider could have planted anything — the validation runs on every
// decode, not just at mint time.
func VeoOperationName(id string) (string, error) {
	if !IsVeoJobID(id) {
		return "", fmt.Errorf("not a veo job id")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, veoIDPrefix))
	if err != nil {
		return "", fmt.Errorf("malformed veo job id: %w", err)
	}
	name := string(raw)
	if name == "" {
		return "", fmt.Errorf("empty veo operation name")
	}
	if strings.ContainsAny(name, "?#\\%") || strings.Contains(name, "//") {
		return "", fmt.Errorf("veo operation name contains URL metacharacters")
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("veo operation name contains an unsafe path segment")
		}
		for _, r := range seg {
			safe := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
				r == '.' || r == '_' || r == '-'
			if !safe {
				return "", fmt.Errorf("veo operation name contains an unsafe character")
			}
		}
	}
	return name, nil
}

// veoSizeMap is the closed, documented lossy map from OpenAI pixel sizes to
// Veo aspect+resolution (§3b — video is carved out of the round-trip-identity
// standard like image size). An unmapped size is a 400, never a guess.
var veoSizeMap = map[string]struct{ aspect, resolution string }{
	"720x1280":  {"9:16", "720p"},
	"1280x720":  {"16:9", "720p"},
	"1024x1792": {"9:16", "1080p"},
	"1792x1024": {"16:9", "1080p"},
}

// VeoSubmitParams is the parsed canonical submit the encoder translates. The
// caller (the video submit handler) supplies parsed values — the codec never
// re-reads the multipart wire.
type VeoSubmitParams struct {
	Prompt        string
	SecondsInt    int    // 0 = absent → omitted (Veo applies its default)
	Size          string // "" = absent → omitted
	InputRefBytes []byte // nil = absent
	InputRefMime  string
	// ExtraFieldNames are the names of multipart fields OUTSIDE the canonical
	// set. The OpenAI leg forwards them verbatim (native passthrough); this
	// leg is allow-list-only — any extra is a 400 (an unknown field cannot be
	// translated, and forwarding it raw would smuggle unscanned content).
	ExtraFieldNames []string
}

type veoInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type veoInstance struct {
	Prompt string           `json:"prompt"`
	Image  *veoImagePayload `json:"image,omitempty"`
}

type veoImagePayload struct {
	InlineData veoInlineData `json:"inlineData"`
}

type veoParameters struct {
	DurationSeconds int    `json:"durationSeconds,omitempty"`
	AspectRatio     string `json:"aspectRatio,omitempty"`
	Resolution      string `json:"resolution,omitempty"`
}

type veoSubmitBody struct {
	Instances  []veoInstance  `json:"instances"`
	Parameters *veoParameters `json:"parameters,omitempty"`
}

// EncodeVeoSubmit translates the canonical submit into the Veo
// `:predictLongRunning` JSON body. coercions lists the caller-visible
// X-Nexus-Coerced markers (the size map is lossy by design).
func EncodeVeoSubmit(p VeoSubmitParams) (body []byte, coercions []string, err error) {
	if len(p.ExtraFieldNames) > 0 {
		return nil, nil, fmt.Errorf("field %q is not supported for this video model", p.ExtraFieldNames[0])
	}
	inst := veoInstance{Prompt: p.Prompt}
	if p.InputRefBytes != nil {
		inst.Image = &veoImagePayload{InlineData: veoInlineData{
			MimeType: p.InputRefMime,
			Data:     base64.StdEncoding.EncodeToString(p.InputRefBytes),
		}}
	}
	params := &veoParameters{}
	if p.SecondsInt > 0 {
		// Veo owns its duration enum (4/6/8 today) — a value it rejects
		// relays as the provider's own 400, same posture as the OpenAI leg.
		params.DurationSeconds = p.SecondsInt
	}
	if p.Size != "" {
		m, ok := veoSizeMap[p.Size]
		if !ok {
			return nil, nil, fmt.Errorf("size %q has no Veo mapping (supported: 720x1280, 1280x720, 1024x1792, 1792x1024)", p.Size)
		}
		params.AspectRatio = m.aspect
		params.Resolution = m.resolution
		coercions = append(coercions,
			fmt.Sprintf("size:%s→aspectRatio %s+resolution %s", p.Size, m.aspect, m.resolution))
	}
	if params.DurationSeconds == 0 && params.AspectRatio == "" {
		params = nil
	}
	body, err = json.Marshal(veoSubmitBody{Instances: []veoInstance{inst}, Parameters: params})
	if err != nil {
		return nil, nil, fmt.Errorf("encode veo submit: %w", err)
	}
	return body, coercions, nil
}

// veoRetention is Veo's server-side artifact retention (2 days). Veo returns
// no expires_at, so the codec synthesizes one from the job's creation time —
// the caller still sees a truthful window on the canonical object.
const veoRetention = 48 * time.Hour

// veoOperation is the LRO envelope subset the decoder reads.
type veoOperation struct {
	Name  string `json:"name"`
	Done  bool   `json:"done"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Response *struct {
		GenerateVideoResponse struct {
			GeneratedSamples []struct {
				Video struct {
					URI string `json:"uri"`
				} `json:"video"`
			} `json:"generatedSamples"`
		} `json:"generateVideoResponse"`
	} `json:"response"`
}

// VeoJobView is the decoded, canonical-facing view of one LRO poll.
type VeoJobView struct {
	// ClientBody is the canonical OpenAI-shaped video job object relayed to
	// the caller.
	ClientBody []byte
	// Status is the canonical lifecycle status (in_progress / completed /
	// failed — the LRO has no named intermediate states, so queued never
	// appears on this leg).
	Status string
	// ExpiresAt is the synthesized retention deadline.
	ExpiresAt time.Time
	// VideoURI is the provider-supplied artifact URI ("" until completed) —
	// the D-V5b download leg fetches it under the SSRF + host-allow-list
	// guard.
	VideoURI string
}

// canonicalVideoObject is the OpenAI-shaped job object this leg synthesizes.
// progress + seconds are always present so an OpenAI-SDK client (whose typed
// model treats them as non-null) parses a Veo job the same as a Sora one.
// completed_at is omitted while running (the OpenAI schema allows null); size
// and prompt are honest Veo-leg omissions (§6 — the LRO carries neither and
// the correlation row stores no prompt text).
type canonicalVideoObject struct {
	ID          string          `json:"id"`
	Object      string          `json:"object"`
	Model       string          `json:"model"`
	Status      string          `json:"status"`
	Progress    int             `json:"progress"`
	Seconds     int             `json:"seconds"`
	CreatedAt   int64           `json:"created_at"`
	CompletedAt int64           `json:"completed_at,omitempty"`
	ExpiresAt   int64           `json:"expires_at"`
	Error       *canonicalError `json:"error,omitempty"`
}

type canonicalError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// DecodeVeoOperation maps one LRO body onto the canonical video object.
// canonicalID is the veo_-encoded job id (the client's handle); model is the
// gateway model id from the job row; requestedSeconds is the billed duration
// (echoed as the canonical `seconds`); createdAt is the job row's creation
// time (the LRO carries no timestamps the retention window could be derived
// from).
func DecodeVeoOperation(lroBody []byte, canonicalID, model string, requestedSeconds int, createdAt time.Time) (VeoJobView, error) {
	var op veoOperation
	if err := json.Unmarshal(lroBody, &op); err != nil {
		return VeoJobView{}, fmt.Errorf("decode veo operation: %w", err)
	}
	view := VeoJobView{ExpiresAt: createdAt.Add(veoRetention)}
	obj := canonicalVideoObject{
		ID:        canonicalID,
		Object:    "video",
		Model:     model,
		Seconds:   requestedSeconds,
		CreatedAt: createdAt.Unix(),
		ExpiresAt: view.ExpiresAt.Unix(),
	}
	switch {
	case !op.Done:
		// The LRO has no named intermediate states — in_progress, never
		// queued (§3b).
		view.Status = "in_progress"
	case op.Error != nil:
		view.Status = "failed"
		obj.Error = &canonicalError{
			Code:    fmt.Sprintf("veo_%d", op.Error.Code),
			Message: op.Error.Message,
		}
	default:
		// done + response. Veo returns done:true with an EMPTY generatedSamples
		// list when every candidate was safety-filtered (raiMediaFilteredCount)
		// — there is no artifact and Veo charges only on success, so this is a
		// FAILED render, not a completed one. Mapping it to completed would
		// dead-end the download (permanent VIDEO_VEO_DECODE_FAILED) AND debit
		// the full seconds×price for a video the provider never delivered.
		if op.Response != nil && len(op.Response.GenerateVideoResponse.GeneratedSamples) > 0 &&
			op.Response.GenerateVideoResponse.GeneratedSamples[0].Video.URI != "" {
			view.Status = "completed"
			view.VideoURI = op.Response.GenerateVideoResponse.GeneratedSamples[0].Video.URI
		} else {
			view.Status = "failed"
			obj.Error = &canonicalError{
				Code:    "veo_no_output",
				Message: "the render produced no downloadable video (it may have been safety-filtered)",
			}
		}
	}
	obj.Status = view.Status
	if view.Status == "completed" {
		obj.Progress = 100
	}
	// completed_at is left absent (omitempty): the Veo LRO reports no
	// completion timestamp and the codec will not fabricate one — an honest
	// null, which the OpenAI video schema permits.
	body, err := json.Marshal(obj)
	if err != nil {
		return VeoJobView{}, fmt.Errorf("encode canonical video object: %w", err)
	}
	view.ClientBody = body
	return view, nil
}

// VeoOperationNameFromSubmit extracts the LRO name from a submit response.
func VeoOperationNameFromSubmit(respBody []byte) (string, error) {
	var op veoOperation
	if err := json.Unmarshal(respBody, &op); err != nil {
		return "", fmt.Errorf("decode veo submit response: %w", err)
	}
	if op.Name == "" {
		return "", fmt.Errorf("veo submit response carries no operation name")
	}
	return op.Name, nil
}
