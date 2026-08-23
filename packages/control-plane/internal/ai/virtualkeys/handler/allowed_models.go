// allowed_models.go — validation of VirtualKey.allowedModels at the admin write
// boundary. The gateway compares these refs EXACTLY, so what is accepted here
// decides what an operator can express, and the model-access picker decides
// what they can see.
package virtualkey

import (
	"fmt"
	"strings"

	"github.com/goccy/go-json"
)

// allowedModelRef mirrors the AI Gateway's store.AllowedModelRef — the exact
// shape the gateway unmarshals the persisted VirtualKey.allowedModels column
// into. CP stores that column verbatim (json.RawMessage), so a create/update
// body carrying any other shape is persisted intact and only fails later, at
// the gateway, where an unparseable value 401s every request the VK makes with
// an opaque decoder error. Validating the shape here — at the write boundary —
// converts that deferred, cryptic failure into an immediate, clear 400.
type allowedModelRef struct {
	ProviderID string `json:"providerId"`
	ModelID    string `json:"modelId"`
}

// validateAllowedModels returns a non-empty error message when raw is not a
// JSON array of {providerId, modelId} objects with non-empty values. A nil/empty
// value or an empty array means "no restriction" and is valid. Extra object keys
// are allowed (the gateway ignores them). Decoding uses the same goccy/go-json
// the gateway uses, so acceptance here guarantees the gateway can parse it.
func validateAllowedModels(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var refs []allowedModelRef
	if err := json.Unmarshal(raw, &refs); err != nil {
		return "allowedModels must be a JSON array of {providerId, modelId} objects"
	}
	for i, r := range refs {
		if r.ProviderID == "" || r.ModelID == "" {
			return fmt.Sprintf("allowedModels[%d] requires a non-empty providerId and modelId", i)
		}
		// A '*' is refused rather than accepted-and-ignored. The gateway compares
		// these refs exactly, so a pattern would silently match nothing — and the
		// model-access picker decides each checkbox by exact equality, so a
		// pattern renders as "0 model(s) selected" while the operator believes
		// they granted something. Both readings are wrong in a way the admin
		// cannot see, which is the failure mode worth a 400.
		//
		// To authorise several models, select them; that is what the picker
		// writes and what an auditor can read back.
		if strings.Contains(r.ModelID, "*") || strings.Contains(r.ProviderID, "*") {
			return fmt.Sprintf("allowedModels[%d] must name a concrete provider and model — "+
				"'*' is not a wildcard here. Select the models you mean; the gateway "+
				"compares these entries exactly and the admin UI cannot display a pattern.", i)
		}
	}
	return ""
}
