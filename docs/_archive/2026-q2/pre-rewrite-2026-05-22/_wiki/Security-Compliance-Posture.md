# Security Compliance Posture

*Audience: compliance leads, procurement teams, and security reviewers evaluating Nexus Gateway for regulated environments.*

Nexus Gateway is not certified against any compliance framework. The architecture is designed so that the technical controls required by SOC 2 Type II, ISO 27001, and HIPAA are present and operational — but certification requires an independent auditor engagement that has not been performed. This page describes current architectural readiness by control domain, identifies gaps, and explains what Nexus does and does not provide for regulated-environment deployments.

---

## What Nexus Gateway provides (and does not provide)

Nexus Gateway is a technical enforcement layer. It provides:

- Audit records of every AI traffic request across all three traffic paths (AI Gateway, Compliance Proxy, Desktop Agent).
- Hook-based policy enforcement with configurable redact, block, and flag decisions.
- PII detection and redaction on request and response bodies before storage.
- Access control via IAM with resource/action/policy model and SSO federation.
- Encryption at rest for provider credentials (AES-256-GCM) and virtual key secrets (HMAC-SHA256 hashed).
- Audit log integrity via hash-chaining on `AdminAuditLog`.
- SIEM forwarding to external compliance systems.
- Data retention configuration with automated purge jobs.

Nexus Gateway does not:

- Make legal determinations about data transfer adequacy or lawfulness.
- Execute or manage Standard Contractual Clauses (SCCs).
- Perform PIPL security assessments.
- Substitute for a DPIA (Data Protection Impact Assessment).
- Provide multi-region data residency (single-node deployment; database localization is the operator's responsibility).

Enterprise legal counsel is responsible for determining whether a particular deployment satisfies applicable regulatory requirements.

## SOC 2 control domains

SOC 2 Type II covers five Trust Service Criteria. The architecture addresses controls in each domain:

| Criterion | Relevant controls |
|---|---|
| **Security (CC6-CC9)** | AES-256-GCM credential encryption; HMAC-SHA256 VK hashing; OAuth 2.0 + PKCE admin auth; mTLS agent enrollment; IAM with least-privilege roles; secrets env-only (no YAML); ≥95% test coverage gate |
| **Availability (A1)** | Fail-open posture on hook errors and control-plane outages; emergency passthrough with auto-revert; kill switch with sub-second propagation; Hub-centric pull-only config sync |
| **Confidentiality (C1)** | PII redaction at emit time; body storage tiering to S3; per-tenant retention config; DSAR endpoint for erasure requests; data localization is operator-controlled |
| **Processing Integrity (PI1)** | Hash-chained `AdminAuditLog`; every bypassed request emits a `traffic_event` with mandatory `bypass_reason`; credential decryption scoped to request lifetime |
| **Privacy (P1-P8)** | PII redaction primitives (hash / token / mask); data retention purge jobs; DSAR admin endpoints; `Authorization` header always masked at audit emit |

## ISO 27001 control areas

ISO 27001:2022 Annex A controls addressed by the architecture:

- **A.5 Information security policies** — configuration binding rules in `CLAUDE.md`; mandatory dev workflow enforced by CI.
- **A.8 Asset management** — provider credentials in the `Credential` table with org-scoped access; audit trail of all credential operations.
- **A.9 Access control** — IAM model with resource NRNs, action catalog, and `iamMW` middleware; no admin action without IAM gate.
- **A.10 Cryptography** — AES-256-GCM for provider credentials; HMAC-SHA256 for VK hashing; TLS 1.2+ on all external surfaces; mTLS for agent enrollment.
- **A.12 Operations security** — audit pipeline with MQ fallback and NDJSON spool; data retention job with observability counters; SIEM forwarding.
- **A.14 System acquisition, development** — ≥95% test coverage binding; code-doc lockstep enforcement; IAM impact review on API changes; SDD pipeline.
- **A.16 Information security incident management** — kill switch and emergency passthrough for operational incidents; audit trail for forensics; SIEM forwarding for off-system evidence.
- **A.17 Business continuity** — fail-open posture; Hub reconcile loop; spillstore fallback on body overflow.

## HIPAA technical safeguards

HIPAA Security Rule technical safeguards (45 CFR §164.312) addressed:

| Safeguard | How addressed |
|---|---|
| Access control (§164.312(a)) | IAM resource/action policies; VK scope restriction per project/org; SSO federation with JIT provisioning |
| Audit controls (§164.312(b)) | `traffic_event` on every request; `AdminAuditLog` on every admin action; hash chain; SIEM forwarding |
| Integrity (§164.312(c)) | `AdminAuditLog` hash chain; `SpillRef.sha256` on body overflow |
| Transmission security (§164.312(e)) | TLS on all external surfaces; mTLS for agent enrollment; Compliance Proxy TLS interception with ECDSA P-256 leaf certs |

HIPAA compliance also requires administrative and physical safeguards that are the operator's responsibility (BAA execution, workforce training, facility controls, etc.).

## Data localization

`TrafficEvent` rows and their `traffic_event_payload` companions constitute personal data. The database must be located in a jurisdiction consistent with the data subjects' location and applicable data localization requirements. The current single-EC2 production deployment does not enforce data residency at the infrastructure layer — this is the operator's responsibility when deploying into a regulated environment.

## Current gaps

The following gaps are known and not yet addressed:

- No air-gapped deployment runbook. Operators running Nexus in air-gapped environments (no outbound internet) must configure provider endpoints manually; no documented procedure exists yet.
- DSAR fulfillment is tracked (API creates `dsar_request` rows and emits admin-audit) but the per-row anonymisation job that actually scrubs data is not yet implemented. Manual operator steps are required to fulfill erasure requests.
- Multi-region active-active deployment is architecturally supported (stateless services, Hub-centric shadow) but no deployment topology for it is documented or tested.

---

## Canonical docs

- [`docs/operators/ops/compliance.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/compliance.md) — compliance hook pipeline, data collection, redaction, SIEM integration, and cross-border data flow
- [`docs/developers/architecture/cross-cutting/storage/data-retention-purge-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/storage/data-retention-purge-architecture.md) — retention classes, GDPR/DSAR flows, anonymisation vs deletion
- [`docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md) — audit pipeline end-to-end

**Adjacent wiki pages**: [Security Audit Forensics](Security-Audit-Forensics) · [Security Threat Model](Security-Threat-Model) · [Security Credential Storage](Security-Credential-Storage) · [Control Plane IAM Model](Control-Plane-IAM-Model) · [Feature Audit And SIEM](Feature-Audit-And-SIEM)
