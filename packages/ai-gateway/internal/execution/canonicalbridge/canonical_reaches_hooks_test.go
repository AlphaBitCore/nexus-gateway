// The canonical response contract declares which fields the waist carries.
// Compliance hooks do not read that wire body — they read the struct view the
// normalize codec decodes from it. Those are two components, and nothing until
// now asserted that the second carries what the first declares.
//
// It did not. `choices[].message.refusal` has been in the declared response
// contract all along (SubsetFields, below) and the normalize codec had no
// mention of the word at all, so a refusal turn — which is the WHOLE assistant
// reply, with `content: null` — reached hooks as an empty message. The repo's
// own adapter notes name a shipping model that answers this way: "o3-mini …
// obeys the schema but delivers it in OpenAI's refusal channel with content:
// null."
//
// This gate is DERIVED from the declared contract rather than from a list
// written here. A hand-written list cannot fail for the field its author forgot
// — which is the failure mode being closed, not a hypothetical one.
package canonicalbridge

import (
	"context"
	"strings"
	"testing"

	normcodecs "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// textBearingCanonicalFields maps each DECLARED canonical response field to the
// text a hook must be able to read from it, or to "" for fields that carry no
// scannable text (identifiers, counters, passthrough metadata).
//
// Every entry of the declared contract must appear here. That is the mechanism:
// adding a field to SubsetFields without classifying it fails this test, so the
// question "can compliance see it?" is asked at the moment the channel is
// declared rather than after something leaks.
var textBearingCanonicalFields = map[string]string{
	"choices[].message.content": "the visible answer",
	"choices[].message.refusal": "the decline the model returned instead",

	// Structural / non-text. Listed explicitly so the coverage check below is a
	// statement about the whole contract, not about the half someone remembered.
	"id":                           "",
	"object":                       "",
	"created":                      "",
	"model":                        "",
	"choices":                      "",
	"choices[].index":              "",
	"choices[].message":            "",
	"choices[].message.role":       "",
	"choices[].message.tool_calls": "", // scanned as structured tool args, not flat text
	"choices[].finish_reason":      "",
	"usage":                        "",
	"usage.prompt_tokens":          "",
	"usage.completion_tokens":      "",
	"usage.total_tokens":           "",
	"usage.prompt_tokens_details":  "",
	"usage.prompt_tokens_details.cached_tokens":        "",
	"usage.prompt_tokens_details.audio_tokens":         "",
	"usage.completion_tokens_details":                  "",
	"usage.completion_tokens_details.reasoning_tokens": "",
	"logprobs":           "",
	"system_fingerprint": "",
	"service_tier":       "",
	"nexus.ext":          "",
}

// TestEveryDeclaredCanonicalFieldIsClassified fails when the canonical response
// contract grows a field nobody decided about.
func TestEveryDeclaredCanonicalFieldIsClassified(t *testing.T) {
	_, response, _ := SubsetFields()
	for _, f := range response {
		if _, ok := textBearingCanonicalFields[f]; !ok {
			t.Errorf("the canonical response contract declares %q and this gate does not "+
				"classify it. Decide whether a compliance hook must be able to read it: give "+
				"it sample text to be scanned, or \"\" if it carries none.", f)
		}
	}
	for f := range textBearingCanonicalFields {
		found := false
		for _, d := range response {
			if d == f {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%q is classified here but no longer declared by the canonical contract — "+
				"a stale entry hides a field that really was removed", f)
		}
	}
}

// TestEveryTextBearingCanonicalFieldReachesHooks builds one canonical response
// carrying every declared text channel at once and asserts the projection hooks
// scan contains each of them.
func TestEveryTextBearingCanonicalFieldReachesHooks(t *testing.T) {
	body := `{
	  "id": "chatcmpl-contract",
	  "object": "chat.completion",
	  "model": "o3-mini",
	  "choices": [{
	    "index": 0,
	    "message": {
	      "role": "assistant",
	      "content": "the visible answer",
	      "refusal": "the decline the model returned instead"
	    },
	    "finish_reason": "stop"
	  }]
	}`

	payload, err := normcodecs.SharedOpenAIChat().Normalize(context.Background(), []byte(body), normcore.Meta{
		AdapterType:  "openai",
		Direction:    normcore.DirectionResponse,
		EndpointPath: "/v1/chat/completions",
	})
	if err != nil {
		t.Fatalf("normalize a canonical response: %v", err)
	}
	projected := strings.Join(payload.TextProjection(), "\n")

	for field, want := range textBearingCanonicalFields {
		if want == "" {
			continue
		}
		if !strings.Contains(projected, want) {
			t.Errorf("%s is declared canonical and delivered to the caller, but its text never "+
				"reaches the projection hooks scan.\nprojection: %q\n"+
				"The wire contract and the struct view hooks read have drifted; a scanner cannot "+
				"redact what it is never shown.", field, projected)
		}
	}
}
