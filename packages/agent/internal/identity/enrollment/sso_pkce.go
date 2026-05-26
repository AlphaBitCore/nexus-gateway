package enrollment

import "github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/pkce"

// GeneratePKCE is a thin alias over shared/pkce.Generate kept so the
// flow_test.go and existing call sites continue to compile. The actual
// implementation lives in packages/shared/identity/pkce.
func GeneratePKCE() (verifier, challenge string, err error) {
	return pkce.Generate()
}
