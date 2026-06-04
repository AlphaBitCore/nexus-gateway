package core

// Identity wire structs: Virtual Keys (list rows + the create/regenerate results
// whose plaintext secret is returned exactly once).

// VirtualKey is one row of GET /api/admin/virtual-keys (envelope {data:[...]}).
// Usage/quota are not on this base resource; they come from analytics. VKType
// and VKStatus are the quota/approval-workflow fields (nullable): status drives
// whether a key is revocable (only "active" keys can be revoked).
type VirtualKey struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	KeyPrefix    string  `json:"keyPrefix"`
	SourceApp    string  `json:"sourceApp"`
	Enabled      bool    `json:"enabled"`
	OwnerID      string  `json:"ownerId"`
	ProjectID    *string `json:"projectId"`
	RateLimitRPM *int    `json:"rateLimitRpm"`
	VKType       *string `json:"vkType"`
	VKStatus     *string `json:"vkStatus"`
}

// Status returns the approval-workflow status ("active"/"pending"/"revoked"/
// "rejected"), or "active" when the column is null (legacy rows default active).
func (v VirtualKey) Status() string {
	if v.VKStatus != nil && *v.VKStatus != "" {
		return *v.VKStatus
	}
	return "active"
}

// Revocable reports whether this key can be revoked — only "active" keys can be
// (the revoke endpoint 404s on any other status). The view gates the revoke
// keystroke on this so the operator never burns a prod confirmation on a no-op.
func (v VirtualKey) Revocable() bool { return v.Status() == "active" }

// RegeneratedVK is the result of rotating a Virtual Key's secret. Key is the new
// plaintext, returned exactly once (the server keeps only a hash), so the view
// shows it once and never persists it.
type RegeneratedVK struct {
	ID        string `json:"id"`
	KeyPrefix string `json:"keyPrefix"`
	Key       string `json:"key"`
}

// virtualKeyList is the {data:[...]} envelope wrapping the VK list.
type virtualKeyList struct {
	Data []VirtualKey `json:"data"`
}

// CreatedVK is the result of creating a Virtual Key. Key is the plaintext
// secret — returned exactly once at creation (the server keeps only a hash), so
// the toolkit captures it here and stores it in the keychain.
type CreatedVK struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	KeyPrefix string `json:"keyPrefix"`
	Key       string `json:"key"`
}
