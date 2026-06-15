package extract

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
)

// NonJSONDetector recognises a chat-shaped body whose wire format
// isn't JSON — proto bytes, Google-batchexecute form-encoded
// envelopes, future weird formats. Each detector exposes a cheap
// byte-level signature check (LooksLike) and a full decoder (Decode)
// that returns a ChatDetection compatible with the JSON multi-spec
// probe.
//
// The PatternNormalizer runs detectors AFTER the JSON probe so
// JSON-shape Tier-2 hits stay first-class; only when the JSON probe
// drops below threshold (Tier 1 didn't match and the multi-spec JSON
// pass found nothing) do detectors get a chance. A detector that
// claims with Confidence >= threshold turns a Tier 3 verbatim row
// into a structured KindAIChat audit row — without requiring a new
// per-host adapter normalize method.
//
// Adding a new format = drop one struct that implements the
// interface and append it to NonJSONDetectors below.
//
// ── Confidence scoring rubric (unified with Tier-1) ────────────────
//
// All NonJSONDetector implementations use scoreDetectorSignals to
// turn (signal-recognition-state) into a [0.40, 1.00] confidence
// value that shares the Coordinator's 0.70 threshold with Tier-1's
// scoreTier1Confidence in confidence.go. The shape is the same:
//
//	score = 0.60 (baseline: envelope shape matched + decode succeeded)
//	      + 0.30 × (required_signals_seen / required_total)
//	      + min(0.10, 0.025 × bonus_signals_seen)
//	      − 0.10 × (unknown_signals / max(1, total_observed_signals))
//
// Range [0.50, 1.00]. The 0.60 baseline is higher than Tier-1's 0.50
// because matching a non-JSON envelope (Connect-RPC framing, XSSI
// prefix) is already higher-specificity evidence than recognising a
// generic JSON shape — false positives at this stage are rare.
//
// Each detector declares its own signal sets in detectorSignalSpec:
//   - ConnectRPCProtobufDetector request: required = ConvMessage tag;
//     bonuses = ModelDetails tag, msgCount tiers
//   - ConnectRPCProtobufDetector response: required = ≥1 frame with
//     extractable text; bonuses = frame-count tiers
//   - BatchExecuteDetector request: required = non-empty f.req prompt
//   - BatchExecuteDetector response: required = wrb.fr text;
//     bonuses = frame-count tiers, detected model name
//
// Unknown signals only apply to the protobuf detector (we can count
// unrecognised proto field tags via ConsumeFieldValue). For the
// batchexecute envelope the "unknown" notion isn't meaningful — the
// XSSI prefix + wrb.fr channel match either succeeds or doesn't, and
// extra inter-chunk metadata is part of Google's framing not a drift
// signal.
type NonJSONDetector interface {
	ID() string
	LooksLike(raw []byte) bool
	Decode(raw []byte, direction string) (ChatDetection, bool)
}

// detectorSignalSpec carries the data scoreDetectorSignals needs to
// turn one decoder run into a confidence value.
type detectorSignalSpec struct {
	// requiredSeen / requiredTotal: structural signals the decoder
	// must observe for the parse to count as complete. If
	// requiredTotal is 0, scoring treats required as fully satisfied
	// (some detectors only have bonuses to grade).
	requiredSeen, requiredTotal int
	// bonusSeen: count of additional structural signals observed.
	// Each contributes 0.025 capped at 0.10 — same per-field optional
	// shape as Tier-1.
	bonusSeen int
	// unknownSeen / observedTotal: drift signal. unknownSeen ≤
	// observedTotal; the penalty is 0.10 × (unknownSeen /
	// max(1, observedTotal)) capped at 0.10. Set both to 0 when the
	// detector can't meaningfully count unknowns (batchexecute).
	unknownSeen, observedTotal int
}

// scoreDetectorSignals applies the unified Tier-2 rubric described in
// the NonJSONDetector docstring above. The [0.50, 1.00] range holds by
// construction — required contributes at most +0.30, the bonus is
// capped at +0.10, and the unknown penalty is at most −0.10 (unknownSeen
// ≤ observedTotal in every caller) — so no terminal clamp is needed.
func scoreDetectorSignals(s detectorSignalSpec) float64 {
	score := 0.60
	if s.requiredTotal > 0 {
		score += 0.30 * float64(s.requiredSeen) / float64(s.requiredTotal)
	} else {
		score += 0.30
	}
	bonus := 0.025 * float64(s.bonusSeen)
	if bonus > 0.10 {
		bonus = 0.10
	}
	score += bonus
	if s.observedTotal > 0 {
		score -= 0.10 * float64(s.unknownSeen) / float64(s.observedTotal)
	}
	return score
}

// NonJSONDetectors is the iteration order the PatternNormalizer
// applies. Order from most-distinctive to most-permissive — once a
// detector claims with high confidence, the chain stops walking.
var NonJSONDetectors = []NonJSONDetector{
	&ConnectRPCProtobufDetector{},
	&BatchExecuteDetector{},
}

// Detector 1: Connect-RPC + protobuf (Cursor-style)

// ConnectRPCProtobufDetector recognises the cursor IDE chat wire
// format and any sibling format that uses Buf's Connect-RPC envelope
// (1-byte flag + 4-byte BE length) wrapping a protobuf message with
// a `ConversationMessage`-shaped repeated field (field 2 = repeated
// bytes, each containing field 1 = string + field 2 = role varint).
//
// This is identical-by-shape to the cursor adapter's bespoke
// Normalize. Promoting it to a Tier-2 detector means any new host
// shipping the same wire shape (a hypothetical "cursor clone" or a
// new cursor endpoint we haven't wired an adapter for) gets the
// same structured Messages without code changes.
type ConnectRPCProtobufDetector struct{}

func (ConnectRPCProtobufDetector) ID() string { return "protobuf-connectrpc-chat" }

// LooksLike runs cheap byte-level tests: response bodies look like
// 5-byte envelope frames (flag byte 0x00 / 0x01 followed by a
// big-endian length plausible for the body); request bodies look
// like a bare protobuf with the conversation field tag (field 2,
// BytesType = wire byte 0x12) near the start.
func (ConnectRPCProtobufDetector) LooksLike(raw []byte) bool {
	if len(raw) < 6 {
		return false
	}
	// Connect-RPC envelope sniff: flag byte 0x00 or 0x01, length field
	// fits the remaining body within reason.
	if (raw[0] == 0x00 || raw[0] == 0x01) && len(raw) >= 5 {
		length := int(raw[1])<<24 | int(raw[2])<<16 | int(raw[3])<<8 | int(raw[4])
		if length > 0 && length <= len(raw)-5 {
			return true
		}
	}
	// Bare protobuf request sniff: first byte is the tag for field 2
	// + WireType BytesType (= (2<<3)|2 = 0x12) which is the
	// ConversationMessage tag in GetChatRequest. Cheap, has ~0% false
	// positive on JSON / form / SSE bodies.
	if raw[0] == 0x12 {
		return true
	}
	return false
}

func (d ConnectRPCProtobufDetector) Decode(raw []byte, direction string) (ChatDetection, bool) {
	// Try response framing first (cursor responses are framed).
	if direction == "response" || direction == "" {
		if det, ok := d.decodeResponse(raw); ok {
			return det, true
		}
	}
	// Then request (bare protobuf).
	if direction == "request" || direction == "" {
		if det, ok := d.decodeRequest(raw); ok {
			return det, true
		}
	}
	return ChatDetection{}, false
}

// decodeRequest walks a bare GetChatRequest-shaped protobuf
// (field 2 repeated ConversationMessage; field 7 ModelDetails). See
// cursor adapter normalize.go for the same field layout — this is
// the inline copy so extract has no upward dependency on adapters.
func (ConnectRPCProtobufDetector) decodeRequest(body []byte) (ChatDetection, bool) {
	det := ChatDetection{SpecID: "protobuf-connectrpc-chat"}
	b := body
	msgCount := 0
	unknownTags := 0
	knownTags := 0
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]
		switch {
		case num == 2 && typ == protowire.BytesType:
			msgBytes, mn := protowire.ConsumeBytes(b)
			if mn < 0 {
				return det, false
			}
			b = b[mn:]
			role, text := parseConvMessage(msgBytes)
			if text != "" {
				det.MessageRoles = append(det.MessageRoles, role)
				det.MessageContents = append(det.MessageContents, text)
				if role == "user" {
					det.UserPrompts = append(det.UserPrompts, text)
				}
				msgCount++
			}
			knownTags++
		case num == 7 && typ == protowire.BytesType:
			mdBytes, mn := protowire.ConsumeBytes(b)
			if mn < 0 {
				return det, false
			}
			b = b[mn:]
			det.Model = parseModelDetailsName(mdBytes)
			knownTags++
		default:
			nn := protowire.ConsumeFieldValue(num, typ, b)
			if nn < 0 {
				return det, false
			}
			b = b[nn:]
			unknownTags++
		}
	}
	if msgCount == 0 {
		return det, false
	}
	bonus := 0
	if det.Model != "" {
		bonus++
	}
	if msgCount >= 2 {
		bonus++
	}
	if msgCount >= 4 {
		bonus++
	}
	det.Confidence = scoreDetectorSignals(detectorSignalSpec{
		requiredSeen:  1, // at least one ConversationMessage
		requiredTotal: 1,
		bonusSeen:     bonus,
		unknownSeen:   unknownTags,
		observedTotal: knownTags + unknownTags,
	})
	return det, true
}

// decodeResponse walks Connect-RPC envelope frames and concatenates
// the field-1 text delta from each StreamChatResponse payload.
func (ConnectRPCProtobufDetector) decodeResponse(body []byte) (ChatDetection, bool) {
	det := ChatDetection{
		SpecID:     "protobuf-connectrpc-chat",
		IsResponse: true,
		IsStream:   true,
	}
	r := bytes.NewReader(body)
	frames := 0
	var assistantText strings.Builder
	for {
		flags, payload, err := streaming.ReadConnectRPCFrame(r)
		if err != nil {
			break
		}
		if len(payload) > 0 {
			p := streaming.MaybeGunzipConnectFrame(flags, payload)
			if t := parseStreamChatResponseFieldOne(p); t != "" {
				assistantText.WriteString(t)
				frames++
			}
		}
		if flags&streaming.ConnectFlagEndStream != 0 {
			break
		}
	}
	if frames == 0 {
		// Try bare-payload fallback.
		if t := parseStreamChatResponseFieldOne(body); t != "" {
			assistantText.WriteString(t)
			frames = 1
		}
	}
	if assistantText.Len() == 0 {
		return det, false
	}
	det.AssistantText = assistantText.String()
	det.MessageRoles = []string{"assistant"}
	det.MessageContents = []string{det.AssistantText}
	bonus := 0
	if frames >= 2 {
		bonus++
	}
	if frames >= 3 {
		bonus++
	}
	det.Confidence = scoreDetectorSignals(detectorSignalSpec{
		requiredSeen:  1, // at least one frame with extractable text
		requiredTotal: 1,
		bonusSeen:     bonus,
	})
	return det, true
}

// parseConvMessage decodes a ConversationMessage protobuf:
//
//	field 1 (string) → text
//	field 2 (varint) → role (1=user, 2=assistant)
func parseConvMessage(msg []byte) (role, text string) {
	role = "user"
	b := msg
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType:
			s, mn := protowire.ConsumeBytes(b)
			if mn < 0 {
				return role, text
			}
			b = b[mn:]
			text = string(s)
		case num == 2 && typ == protowire.VarintType:
			v, mn := protowire.ConsumeVarint(b)
			if mn < 0 {
				return role, text
			}
			b = b[mn:]
			switch v {
			case 1:
				role = "user"
			case 2:
				role = "assistant"
			}
		default:
			nn := protowire.ConsumeFieldValue(num, typ, b)
			if nn < 0 {
				return role, text
			}
			b = b[nn:]
		}
	}
	return role, text
}

// parseModelDetailsName extracts the model name (field 1 string)
// from a ModelDetails sub-message.
func parseModelDetailsName(md []byte) string {
	b := md
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]
		if num == 1 && typ == protowire.BytesType {
			s, mn := protowire.ConsumeBytes(b)
			if mn < 0 {
				return ""
			}
			return string(s)
		}
		n = protowire.ConsumeFieldValue(num, typ, b)
		if n < 0 {
			break
		}
		b = b[n:]
	}
	return ""
}

// parseStreamChatResponseFieldOne reads the string at protobuf field 1
// in a StreamChatResponse-shaped payload.
func parseStreamChatResponseFieldOne(payload []byte) string {
	b := payload
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return ""
		}
		b = b[n:]
		if num == 1 && typ == protowire.BytesType {
			s, mn := protowire.ConsumeBytes(b)
			if mn < 0 {
				return ""
			}
			return string(s)
		}
		nn := protowire.ConsumeFieldValue(num, typ, b)
		if nn < 0 {
			return ""
		}
		b = b[nn:]
	}
	return ""
}

// Detector 2: Google batchexecute (Gemini-web-style)

// BatchExecuteDetector recognises Google's internal batchexecute
// envelope used by gemini.google.com (and other Google web apps —
// Translate, Docs, Maps interact-with-AI features). The format has
// two cheap signatures:
//
//   - Request: form-urlencoded body with an `f.req=` field carrying
//     a double-JSON-encoded array; first inner element's first item
//     is the user prompt.
//   - Response: body starts with the XSSI guard `)]}'` then a
//     sequence of <length>\n<chunk>\n frames, each chunk a
//     [["wrb.fr", null, "<INNER-JSON>"]] array whose inner JSON's
//     [4][0][1][0] path carries the cumulative assistant text.
type BatchExecuteDetector struct{}

func (BatchExecuteDetector) ID() string { return "google-batchexecute-chat" }

func (BatchExecuteDetector) LooksLike(raw []byte) bool {
	probe := raw
	if len(probe) > 256 {
		probe = probe[:256]
	}
	s := strings.TrimLeft(string(probe), " \r\n\t")
	if strings.HasPrefix(s, "f.req=") {
		return true
	}
	if strings.Contains(s, "&f.req=") {
		return true
	}
	if strings.HasPrefix(s, ")]}'") {
		return true
	}
	return false
}

func (d BatchExecuteDetector) Decode(raw []byte, direction string) (ChatDetection, bool) {
	probe := raw
	if len(probe) > 256 {
		probe = probe[:256]
	}
	s := strings.TrimLeft(string(probe), " \r\n\t")
	if strings.HasPrefix(s, ")]}'") {
		return d.decodeResponse(raw)
	}
	if strings.HasPrefix(s, "f.req=") || strings.Contains(s, "&f.req=") {
		return d.decodeRequest(raw)
	}
	return ChatDetection{}, false
}

// decodeRequest pulls the prompt out of f.req[1] (after URL-decode +
// outer JSON parse + inner JSON parse), pulling the string at [0][0].
func (BatchExecuteDetector) decodeRequest(body []byte) (ChatDetection, bool) {
	det := ChatDetection{SpecID: "google-batchexecute-chat"}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return det, false
	}
	fReq := form.Get("f.req")
	if fReq == "" {
		return det, false
	}
	var outer []json.RawMessage
	if err := json.Unmarshal([]byte(fReq), &outer); err != nil || len(outer) < 2 {
		return det, false
	}
	var innerStr string
	if err := json.Unmarshal(outer[1], &innerStr); err != nil {
		return det, false
	}
	var inner []json.RawMessage
	if err := json.Unmarshal([]byte(innerStr), &inner); err != nil || len(inner) == 0 {
		return det, false
	}
	var first []json.RawMessage
	if err := json.Unmarshal(inner[0], &first); err != nil || len(first) == 0 {
		return det, false
	}
	var prompt string
	if err := json.Unmarshal(first[0], &prompt); err != nil || prompt == "" {
		return det, false
	}
	det.MessageRoles = []string{"user"}
	det.MessageContents = []string{prompt}
	det.UserPrompts = []string{prompt}
	det.Confidence = scoreDetectorSignals(detectorSignalSpec{
		requiredSeen:  1, // f.req → outer → inner → first → prompt all succeeded
		requiredTotal: 1,
	})
	return det, true
}

// decodeResponse walks the response stream after the `)]}'` XSSI
// prefix and extracts the final cumulative assistant text from the
// last `wrb.fr` chunk that carried one.
//
// We DELIBERATELY ignore the `<length>\n` headers Google sprinkles
// between chunks. In observed prod captures, the length count for
// the first chunk is off by 1–2 bytes compared to the actual JSON
// content size (it appears to count a leading blank-line newline
// after `)]}'` as part of chunk 1's length). Trying to honour the
// reported length leaves the parser one byte misaligned and every
// subsequent chunk fails to unmarshal.
//
// json.Decoder solves this without depending on the lengths at all:
// it reads one whole JSON value at a time from the stream, skipping
// inter-value whitespace, and naturally handles the length-line
// numbers as standalone numeric values that we can ignore. Robust
// to format drift — if Google adjusts the length semantics again,
// or interleaves new non-JSON markers, the parser still picks up
// every `[["wrb.fr", ...]]` chunk on the stream.
func (BatchExecuteDetector) decodeResponse(body []byte) (ChatDetection, bool) {
	det := ChatDetection{
		SpecID:     "google-batchexecute-chat",
		IsResponse: true,
		IsStream:   true,
	}
	rest := bytes.TrimLeft(body, " \r\n\t")
	if !bytes.HasPrefix(rest, []byte(")]}'")) {
		return det, false
	}
	rest = rest[len(")]}'"):]

	dec := json.NewDecoder(bytes.NewReader(rest))
	var finalText, modelName string
	frames := 0
	for {
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			break
		}
		// Length-line numbers (177, 1513, …) decode as JSON numbers;
		// only array values are chunk payloads we care about. Cheap
		// shape gate on the first non-whitespace byte avoids the
		// allocation of a failed inner-unmarshal.
		v := bytes.TrimSpace(value)
		if len(v) == 0 || v[0] != '[' {
			continue
		}
		text, model := extractFromBatchChunk(value)
		if text != "" {
			finalText = text
		}
		if model != "" && modelName == "" {
			modelName = model
		}
		frames++
	}
	if finalText == "" {
		return det, false
	}
	det.AssistantText = finalText
	det.MessageRoles = []string{"assistant"}
	det.MessageContents = []string{finalText}
	det.Model = modelName
	bonus := 0
	if frames >= 2 {
		bonus++
	}
	if frames >= 4 {
		bonus++
	}
	if modelName != "" {
		bonus++
	}
	det.Confidence = scoreDetectorSignals(detectorSignalSpec{
		requiredSeen:  1, // wrb.fr chunk produced assistant text
		requiredTotal: 1,
		bonusSeen:     bonus,
	})
	return det, true
}

// extractFromBatchChunk + extractFromBatchInner are the inline-copied
// implementations of the same helpers in
// packages/shared/traffic/adapters/geminiweb/normalize.go — kept in
// the extract package so the detector has no upward dependency on
// adapters.
func extractFromBatchChunk(chunk []byte) (text, model string) {
	var outerArr []json.RawMessage
	if err := json.Unmarshal(chunk, &outerArr); err != nil {
		return "", ""
	}
	for _, row := range outerArr {
		var entry []json.RawMessage
		if err := json.Unmarshal(row, &entry); err != nil || len(entry) < 3 {
			continue
		}
		var channel string
		if err := json.Unmarshal(entry[0], &channel); err != nil || channel != "wrb.fr" {
			continue
		}
		var innerStr string
		if err := json.Unmarshal(entry[2], &innerStr); err != nil {
			continue
		}
		t, m := extractFromBatchInner([]byte(innerStr))
		if t != "" {
			text = t
		}
		if m != "" && model == "" {
			model = m
		}
	}
	return text, model
}

func extractFromBatchInner(inner []byte) (text, model string) {
	var arr []json.RawMessage
	if err := json.Unmarshal(inner, &arr); err != nil || len(arr) < 5 {
		return "", ""
	}
	// Path A — small wrb.fr replies put the text inside
	// arr[4][0][1][0] (candidate wrapper).
	if candidates := []json.RawMessage{}; json.Unmarshal(arr[4], &candidates) == nil && len(candidates) > 0 {
		var cand []json.RawMessage
		if err := json.Unmarshal(candidates[0], &cand); err == nil && len(cand) >= 2 {
			var textArr []json.RawMessage
			if err := json.Unmarshal(cand[1], &textArr); err == nil && len(textArr) > 0 {
				var t string
				if err := json.Unmarshal(textArr[0], &t); err == nil {
					text = t
				}
			}
			if m := scanForModelString(cand); m != "" {
				model = m
			}
		}
	}
	// Path B — larger reply chunks (gemini.google.com's final delta
	// frame, observed in prod traffic_event d64abfd9 chunk 3) flatten
	// the metadata into the top-level inner array — model name sits at
	// arr[42] alongside other slot fields, and the assistant text lives
	// at arr[26] in a deeper [[[[null,[null,0,"<text>"]]]]] structure.
	// We fall back to flat scanning so both shapes work without
	// hard-coding indices.
	if model == "" {
		model = scanForModelString(arr)
	}
	if text == "" {
		text = scanForLongestText(arr)
	}
	return text, model
}

// scanForModelString returns the first short string in `vals` that
// looks like a Gemini model name (Flash / Pro / Ultra / Nano).
func scanForModelString(vals []json.RawMessage) string {
	for _, raw := range vals {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			continue
		}
		if len(s) == 0 || len(s) > 32 {
			continue
		}
		lower := strings.ToLower(s)
		if strings.Contains(lower, "flash") ||
			strings.Contains(lower, " pro") ||
			strings.Contains(lower, "ultra") ||
			strings.Contains(lower, "nano") {
			return s
		}
	}
	return ""
}

// scanForLongestText recurses into the inner-array values looking for
// the deepest nested string that resembles assistant text — used as a
// fallback when the well-known arr[4][0][1][0] candidate-wrapper path
// is empty because the chunk is the "final delta" with a flattened
// shape. The longest plausible text wins (heuristic: assistant body
// is typically the longest string in the chunk).
func scanForLongestText(vals []json.RawMessage) string {
	var best string
	var walk func(raw json.RawMessage)
	walk = func(raw json.RawMessage) {
		v := bytes.TrimSpace(raw)
		if len(v) == 0 {
			return
		}
		switch v[0] {
		case '"':
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return
			}
			if len(s) > len(best) && len(s) >= 16 {
				best = s
			}
		case '[':
			var arr []json.RawMessage
			if err := json.Unmarshal(v, &arr); err != nil {
				return
			}
			for _, el := range arr {
				walk(el)
			}
		}
	}
	for _, el := range vals {
		walk(el)
	}
	return best
}
