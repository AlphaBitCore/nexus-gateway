package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	// stdlib encoding/json, not goccy: this digest must be byte-stable across
	// runs, and stdlib guarantees map keys are marshalled in sorted order.
	"encoding/json"
	"strconv"
	"strings"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// answerKey digests the request inputs that change WHAT the model answers but
// are not part of the text it answers about.
//
// The L2 semantic cache retrieves by vector similarity over the message text
// alone, and both the entry key and the FT.SEARCH filter are built from
// scope / response kind / provider / model. Nothing carried the caller's
// sampling settings, so two requests differing only in `temperature` — one
// pinned to 0 for determinism, one at 2 for variety — landed on the same
// entry and were served each other's answer. The same held for `top_p`,
// `top_k`, `max_tokens`, `stop`, and the presence of a tools array.
//
// Folding the digest into the entry's validity conditions makes those
// requests distinct: a caller who pinned temperature=0 can still hit their
// OWN earlier temperature=0 answers, which keying by "no sampling params at
// all" or skipping L2 for them would have taken away.
//
// The digest is computed ONCE per request from the canonical payload and the
// same string is handed to both the write and the read path. That is
// deliberate: a writer and a reader that each derive the key from their own
// inputs can drift, and the failure mode of drift is a permanent 100% L2
// miss — quieter than the bug being fixed, and harder to notice.
//
// Known residual: the canonical carries no `response_format`, so JSON mode
// and free text still share a key. That is a gap in the canonical, not in
// this digest; when the field lands here it belongs in the list below.
func answerKey(np *normcore.NormalizedPayload) string {
	if np == nil {
		return ""
	}

	var sb strings.Builder
	// A NUL separator keeps concatenation unambiguous: no component can
	// contain one, so distinct field sets cannot produce the same string.
	writeField := func(name, value string) {
		sb.WriteString(name)
		sb.WriteByte(0)
		sb.WriteString(value)
		sb.WriteByte(0)
	}

	if p := np.Params; p != nil {
		// Absent and present-but-default are NOT equivalent here. A caller
		// who sends temperature=1 explicitly and one who sends nothing may
		// get the same answer from the upstream today, but the gateway's own
		// backstops can fill an absent field with something else, so the two
		// are kept apart rather than assumed interchangeable.
		if p.Temperature != nil {
			writeField("temperature", strconv.FormatFloat(*p.Temperature, 'g', -1, 64))
		}
		if p.TopP != nil {
			writeField("top_p", strconv.FormatFloat(*p.TopP, 'g', -1, 64))
		}
		if p.TopK != nil {
			writeField("top_k", strconv.Itoa(*p.TopK))
		}
		if p.MaxTokens != nil {
			writeField("max_tokens", strconv.Itoa(*p.MaxTokens))
		}
		for _, s := range p.Stop {
			writeField("stop", s)
		}
	}

	// Tool definitions change the answer even when no tool is called: the
	// model may answer differently knowing a tool exists. Name and parameter
	// schema both matter — same name with a different schema is a different
	// capability. Order is taken as sent; callers that reorder their tool
	// list get a distinct key, which costs a miss rather than a wrong answer.
	for _, t := range np.Tools {
		writeField("tool", t.Name)
		if len(t.ParametersJSONSchema) > 0 {
			// Marshalled through the canonical JSON encoder so map iteration
			// order cannot leak into the digest — Go randomizes it, and a key
			// that varied run to run would miss its own entries.
			schema, err := json.Marshal(t.ParametersJSONSchema)
			if err != nil {
				// An unmarshallable schema is not a reason to silently widen
				// the key; fall back to a marker so such requests stay
				// distinct from tool-free ones without pretending to describe
				// the schema.
				writeField("tool_schema", "\x01unencodable")
			} else {
				writeField("tool_schema", string(schema))
			}
		}
	}

	if sb.Len() == 0 {
		// No answer-affecting parameters at all. Returning "" rather than the
		// digest of an empty string keeps entries written before this existed
		// reachable, so the default-parameter traffic that is the bulk of the
		// corpus does not take a cold-cache hit for a bug it never had.
		return ""
	}

	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])[:32]
}

// l2Canonical bundles the two values every L2 write path needs from ONE
// canonical payload: the messages that become the embedding input, and the
// digest of the parameters that decide which entry those messages may reach.
//
// They travel together as a single value on purpose. Passing the messages as
// one argument and the key as another lets a call site supply the first and
// forget the second, and the resulting failure — a writer whose key disagrees
// with the reader's — is a permanent, silent 100% L2 miss.
type l2Canonical struct {
	msgs      []normcore.Message
	answerKey string
}

// l2CanonicalFrom derives both values from a single payload. Nil-safe: a
// request with no canonical yields the zero value, which the write path
// already treats as "nothing to embed".
func l2CanonicalFrom(np *normcore.NormalizedPayload) l2Canonical {
	if np == nil {
		return l2Canonical{}
	}
	return l2Canonical{msgs: np.Messages, answerKey: answerKey(np)}
}
