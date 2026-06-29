package audit

// The identity and details JSONB objects were historically built as
// map[string]any and marshaled by the wire encoder via go-json's map reflection
// path: it sorts keys, iterates, and type-switches every boxed value on every
// event. On the gateway audit hot path that reflection dominated traffic-event
// serialization. These typed structs produce JSON that is key-for-key equivalent
// (object key order differs — declaration order vs the map path's sorted order —
// which is irrelevant for the JSONB columns they land in) but marshal through
// go-json's compiled struct encoder: no key sort, no map iteration, no per-field
// interface boxing for the scalar fields. buildIdentity / buildDetails are the
// single source of the wire shape; see recordToMessage.

// idName is the {id,name} pair used for each resolved identity subtree entry.
type idName struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// identityWire is the typed form of the traffic_event.identity object. A subtree
// pointer is non-nil (and thus emitted) only when its foreign key resolved, which
// reproduces the old map's conditional key insertion exactly via omitempty.
type identityWire struct {
	VK            *idName `json:"vk,omitempty"`
	User          *idName `json:"user,omitempty"`
	Project       *idName `json:"project,omitempty"`
	APICredential *idName `json:"apiCredential,omitempty"`
	Status        string  `json:"status"`
}

// detailsWire is the typed form of the traffic_event.details object. The ten base
// fields carry no omitempty so they are always present (matching the old map,
// which always inserted them — empty strings as "" and absent any-values as
// null). The four hook-rewrite fields are pointers with omitempty so they are
// present iff their stage rewrote, reproducing the old map's conditional blocks
// (including hookRewriteCount:0 when a rewrite happened to leave the count zero).
type detailsWire struct {
	RequestID              string `json:"requestId"`
	ClientRequestID        string `json:"clientRequestId"`
	SourceApp              string `json:"sourceApp"`
	CacheKey               string `json:"cacheKey"`
	ResponseHookReason     string `json:"responseHookReason"`
	ResponseHookReasonCode string `json:"responseHookReasonCode"`
	RoutingDecision        any    `json:"routingDecision"`
	QualitySignals         any    `json:"qualitySignals"`
	ComplianceFlags        any    `json:"complianceFlags"`
	Metadata               any    `json:"metadata"`

	HookRewritten            *bool `json:"hookRewritten,omitempty"`
	HookRewriteCount         *int  `json:"hookRewriteCount,omitempty"`
	ResponseHookRewritten    *bool `json:"responseHookRewritten,omitempty"`
	ResponseHookRewriteCount *int  `json:"responseHookRewriteCount,omitempty"`
}

// buildIdentity derives the typed identity object from a Record. status is
// "matched" iff at least one owner foreign key (user or project) resolved at
// request time; otherwise "pending" so the Hub IdentityEnricher reattaches an
// owner later via IP lookup.
func buildIdentity(rec *Record) identityWire {
	iw := identityWire{}
	if rec.VirtualKeyID != "" {
		iw.VK = &idName{ID: rec.VirtualKeyID, Name: rec.VirtualKeyName}
	}
	if rec.UserID != "" {
		iw.User = &idName{ID: rec.UserID, Name: rec.UserDisplayName}
	}
	if rec.ProjectID != "" {
		iw.Project = &idName{ID: rec.ProjectID, Name: rec.ProjectName}
	}
	if rec.CredentialID != "" {
		iw.APICredential = &idName{ID: rec.CredentialID, Name: rec.CredentialName}
	}
	if rec.UserID != "" || rec.ProjectID != "" {
		iw.Status = "matched"
	} else {
		iw.Status = "pending"
	}
	return iw
}

// buildDetails derives the typed details object from a Record. The hook-rewrite
// pointers are set (to addressable copies) only when their stage rewrote, so a
// non-rewriting record omits those keys entirely.
func buildDetails(rec *Record) detailsWire {
	dw := detailsWire{
		RequestID:              rec.RequestID,
		ClientRequestID:        rec.ClientRequestID,
		SourceApp:              rec.SourceApp,
		CacheKey:               rec.CacheKey,
		ResponseHookReason:     rec.ResponseHookReason,
		ResponseHookReasonCode: rec.ResponseHookReasonCode,
		RoutingDecision:        rec.RoutingDecision,
		QualitySignals:         rec.QualitySignals,
		ComplianceFlags:        rec.ComplianceFlags,
		Metadata:               rec.Metadata,
	}
	if rec.HookRewritten {
		t := true
		dw.HookRewritten = &t
		c := rec.HookRewriteCount
		dw.HookRewriteCount = &c
	}
	if rec.ResponseHookRewritten {
		t := true
		dw.ResponseHookRewritten = &t
		c := rec.ResponseHookRewriteCount
		dw.ResponseHookRewriteCount = &c
	}
	return dw
}
