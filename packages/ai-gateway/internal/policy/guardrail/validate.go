package guardrail

import (
	"errors"
	"fmt"
	"strings"
)

// NormalizeStage resolves the request stage to its canonical value. An empty
// stage defaults to "input" (the common case: check a prompt); any value other
// than input/output is a client error.
func NormalizeStage(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", StageInput:
		return StageInput, nil
	case StageOutput:
		return StageOutput, nil
	default:
		return "", fmt.Errorf("stage must be %q or %q, got %q", StageInput, StageOutput, s)
	}
}

// Validate enforces the exactly-one-of-content-or-messages input contract on a
// non-whitespace basis. It is the single input gate the handler runs before
// building the pipeline; Segments() may then be called safely (it never returns
// whitespace-only material once Validate passes).
func (r *Request) Validate() error {
	hasContent := strings.TrimSpace(r.Content) != ""
	hasMessages := false
	for i := range r.Messages {
		if strings.TrimSpace(r.Messages[i].Content) != "" {
			hasMessages = true
			break
		}
	}
	switch {
	case hasContent && hasMessages:
		return errors.New("provide exactly one of content or messages, not both")
	case !hasContent && !hasMessages:
		return errors.New("exactly one of content or messages must be non-empty")
	}
	return nil
}
