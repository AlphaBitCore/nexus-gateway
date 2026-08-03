package mq

import "time"

// AdminAuditMessage is the MQ wire format for admin audit log events.
// Published by Control Plane to "nexus.event.admin-audit".
// Consumers (hub-db-writer, hub-alerting) deserialize from this.
//
// The hash chain (previousHash / integrityHash) is computed Hub-side in
// packages/nexus-hub/internal/observability/audit/chain.go; sending hashes on the wire
// would let any CP replica fork the chain. The CP just formats + publishes.
type AdminAuditMessage struct {
	ID             string    `json:"id"`
	Timestamp      time.Time `json:"timestamp"`
	ActorID        string    `json:"actorId"`
	ActorLabel     string    `json:"actorLabel"`
	ActorRole      string    `json:"actorRole"`
	SourceIP       string    `json:"sourceIp,omitempty"`
	Action         string    `json:"action"`
	EntityType     string    `json:"entityType"`
	EntityID       string    `json:"entityId"`
	BeforeState    any       `json:"beforeState,omitempty"`
	AfterState     any       `json:"afterState,omitempty"`
	NexusRequestID string    `json:"nexusRequestId,omitempty"`
	// Via records the channel that initiated the mutation — "assistant" for an
	// AI-initiated admin write performed by the web assistant, empty for a direct
	// human/UI action. The Hub consumer feeds it into the audit hash chain so the
	// AI-attribution marker is tamper-evident.
	Via string `json:"via,omitempty"`
}
