// Package iam provides IAM policy types and test fixtures.
// The Prisma seed at tools/db-migrate/seed/seed.ts is the canonical source
// for managed policies; this file retains only the types used by engine.go /
// validator.go and the policy fixtures used by their tests.
package iam

// PolicyDocument is an IAM policy document (AWS-style).
type PolicyDocument struct {
	Version   string      `json:"Version"`
	Statement []Statement `json:"Statement"`
}

// Statement is a single IAM policy statement.
//
// Action and Resource use StringList so the engine accepts both
// AWS-canonical shapes:
//
//	"Action": "s3:PutObject"            // single string
//	"Action": ["s3:GetObject", "s3:X"]  // array
//
// Round-trip preserves the AWS-canonical shape (length-1 → bare
// string, length>1 → array) when re-serialized.
type Statement struct {
	Sid       string         `json:"Sid,omitempty"`
	Effect    string         `json:"Effect"` // "Allow" or "Deny"
	Action    StringList     `json:"Action"`
	Resource  StringList     `json:"Resource"`
	Condition ConditionBlock `json:"Condition,omitempty"`
}

const PolicyVersion = "2026-05-12"

// NexusSuperAdmin grants every action on every resource — the canonical
// "full administrator" fixture used by engine + validator tests. It is
// not seeded from this constant; the actual seeded super-admin policy
// lives in tools/db-migrate/seed/seed.ts (block 15c,
// NexusAdminFullAccess).
var NexusSuperAdmin = PolicyDocument{
	Version: PolicyVersion,
	Statement: []Statement{{
		Sid:      "FullAccess",
		Effect:   "Allow",
		Action:   []string{"*"},
		Resource: []string{"nrn:nexus:*:*:*/*"},
	}},
}

// NexusViewer grants the read-only action set across every resource defined in
// shared/iam.Catalog. Used as a positive fixture by validator_test.go. New
// catalog resources must add their Read action here to keep the fixture in sync.
//
// Coverage: every catalog resource that defines VerbRead is listed
// below. Resources omitted intentionally are those whose only verb is
// not Read — device-assignment (Update only, internal audit channel),
// device-enrollment (Enroll only, agent SSO), nexus-session (Revoke
// only, force-logout).
var NexusViewer = PolicyDocument{
	Version: PolicyVersion,
	Statement: []Statement{{
		Sid:    "ReadOnly",
		Effect: "Allow",
		Action: []string{
			// AI traffic plane.
			"admin:provider.read", "admin:model.read", "admin:model-pricing.read",
			"admin:credential.read",
			"admin:virtual-key.read", "admin:routing-rule.read",
			"admin:quota-policy.read", "admin:quota-override.read",
			"admin:quota-analytics.read", "admin:analytics.read",
			"admin:traffic-log.read", "admin:prompt-cache.read",
			"admin:passthrough.read",
			// semantic-cache governs the L1 embedding singleton + time-sensitive
			// rule list. Viewer gets read so the Cache Settings page loads for all roles.
			"admin:semantic-cache.read",
			// Compliance plane.
			"admin:hook.read", "admin:rule-pack.read",
			"admin:compliance-exemption.read",
			"admin:compliance-report.read", "admin:interception-domain.read",
			"admin:dsar.read", "admin:payload-capture.read",
			"admin:ai-guard-config.read",
			// Agent fleet.
			"admin:agent-device.read", "admin:device-group.read",
			"admin:device-defaults.read", "admin:agent-attestation.read",
			// Platform ops.
			"admin:alert.read", "admin:observability.read", "admin:observability-dlq.read", "admin:settings.read",
			"admin:diagnostic-mode.read", "admin:node.read",
			// IAM.
			"admin:user.read", "admin:api-key.read",
			"admin:organization.read", "admin:project.read",
			"admin:iam-policy.read", "admin:iam-group.read",
			"admin:audit-log.read", "admin:revocation.read",
			"admin:identity-provider.read",
		},
		Resource: []string{"nrn:nexus:*:*:*/*"},
	}},
}

// NexusRegionalDeviceAdmin is the worked example canned policy for group-scoped
// device management. It grants device CRUD + force-resync + cert rotation,
// scoped to a specific DeviceGroup via the `${nexus:GroupId}` placeholder.
// The admin substitutes the actual group ID at attachment time — the same
// pattern used for `${nexus:OrgId}` in org-scoped policies.
var NexusRegionalDeviceAdmin = PolicyDocument{
	Version: PolicyVersion,
	Statement: []Statement{{
		Effect: "Allow",
		Action: []string{
			"admin:agent-device.read",
			"admin:agent-device.update",
			"admin:agent-device.delete",
			"admin:agent-device.force-resync",
			"admin:agent-device.rotate",
			"admin:diagnostic-mode.read",
		},
		Resource: []string{
			// `${nexus:GroupId}` is substituted at policy-attachment
			// time. The resourceID segment carries `group:<id>/*`
			// which MatchNRN resolves against candidate NRNs
			// BuildDeviceCandidateNRNs emits for member devices.
			"nrn:nexus:agent:*:agent-device/group:${nexus:GroupId}/*",
			"nrn:nexus:platform:*:diagnostic-mode/*",
		},
	}},
}
