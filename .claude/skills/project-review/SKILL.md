# Multi-Role Review Prompt Guide

description: Project review with multiple roles


---

## Overview

This guide contains review prompts for **9 specialist roles**, designed for
a full-system audit of the AI Gateway (Admin Dashboard, client, API,
architecture, security, compliance, etc.).

Findings from every role must be remediated through the standard
implementation workflow:

```
Architecture → Requirements → SDD → OpenAPI Spec → Code → Unit Tests → Verification
```

---

## Common review standards

> The three items below are **mandatory defaults** for every role prompt
> and are embedded in each role's execution flow.

---

### 1. Enable extended thinking

```
Use extended thinking to reason through all findings before responding.
```

Before producing any finding, Claude must internally reason through:
- The root cause and impact chain of the issue
- Cross-module and cross-role ripple effects
- Edge cases and underlying assumptions
- Feasibility and side effects of the proposed fix

---

### 2. Severity-ordered output

Every finding must be classified and ordered by the levels below:

| Level | Marker | Definition |
|-------|--------|------------|
| **Critical** | 🚨 | Systemic risk affecting core function or security; fix immediately |
| **High** | 🔴 | Important missing function or severe UX issue; fix in the current iteration |
| **Medium** | 🟡 | Incomplete function or sub-optimal UX; fix in an upcoming iteration |
| **Low** | 🟢 | Optimization; add to the backlog for opportunistic pickup |

Output order: `Critical → High → Medium → Low`.

---

### 3. JIRA-ready ticket format

Every finding must be rendered in the format below:

```
───────────────────────────────────────────
JIRA TICKET
───────────────────────────────────────────
Title        : [role-prefix]-[number] short problem title
Type         : Gap / Issue / Improvement
Severity     : Critical / High / Medium / Low
Role         : the review role that surfaced the issue

Description  :
  [Clearly describe current behavior, trigger conditions, and context]

Impact       :
  [Describe the impact on users, business, and the system]

Fix Plan     :
  1. Architecture — [architecture-level response]
  2. Requirements — [additions needed in the requirements doc]
  3. SDD          — [updates needed in the software design doc]
  4. OpenAPI      — [API spec changes]
  5. Code         — [implementation guidance and gotchas]
  6. Unit Tests   — [test cases that must be covered]
  7. Verification — [acceptance criteria and verification methods]

Acceptance Criteria :
  - [ ] Condition 1
  - [ ] Condition 2
  - [ ] Condition 3

Estimated Effort : [X] Story Points  |  about [X] person-days
───────────────────────────────────────────
```

---

### Output-type markers

| Marker | Meaning |
|--------|---------|
| 🔴 **Gap** | A missing feature or design element |
| 🟡 **Issue** | An existing defect or problem |
| 🟢 **Improvement** | An optimization suggestion |

---

## Role prompts

---

### Role 1 — Admin Dashboard multi-role UX & functional review

```
Use extended thinking to reason through all findings before responding.

You are a senior UX reviewer simulating ALL admin roles in this dashboard
(e.g., Super Admin, Operator, Viewer, Finance, Support, etc.).

For each role:
1. Walk through the dashboard as that role would realistically use it
2. Identify any UX/UI gaps, missing permissions, confusing flows,
   or functional gaps specific to that role
3. Flag issues that require UX Designer or Product Manager intervention

Rank all findings by severity: Critical > High > Medium > Low.

For each finding, output a JIRA-ready ticket with:
- Title, Type, Severity, Role
- Description & Impact
- Fix Plan: Architecture → Requirements → SDD → OpenAPI → Code → Unit Tests → Verification
- Acceptance Criteria (checklist)
- Estimated Effort (Story Points & person-days)
```

**Review focus:**
- Whether each role's permission boundary is clear
- Cross-role operational conflicts and data visibility
- Whether navigation structure fits each role's working habits
- Role-switch experience and permission hints

---

### Role 2 — End user: client & API functional review

```
Use extended thinking to reason through all findings before responding.

You are an end user who uses this AI gateway to call AI models via
client-side interfaces and APIs.

Review:
- Onboarding & authentication flow
- API key management and usage experience
- Model selection, request/response UX
- Error messages, rate limit handling, quota visibility
- SDK/API documentation clarity and completeness
- Any friction points, missing features, or confusing behaviors

Rank all findings by severity: Critical > High > Medium > Low.

For each finding, output a JIRA-ready ticket with:
- Title, Type, Severity, Role
- Description & Impact
- Fix Plan: Architecture → Requirements → SDD → OpenAPI → Code → Unit Tests → Verification
- Acceptance Criteria (checklist)
- Estimated Effort (Story Points & person-days)
```

**Review focus:**
- Whether the first-use onboarding flow is smooth
- API Key creation, management, and rotation experience
- Whether error messages are clear and actionable
- Visibility of rate limits and quotas
- Consistency between documentation and actual API behavior

---

### Role 3 — Software architect: system design & code review

```
Use extended thinking to reason through all findings before responding.

You are a principal software architect reviewing this system's design and codebase.

Review:
- Overall architecture patterns and soundness
- Service boundaries, coupling, and cohesion
- Scalability and performance bottlenecks
- Tech debt, anti-patterns, and code quality
- Data flow, API design consistency
- Dependency management and extensibility

Rank all findings by severity: Critical > High > Medium > Low.

For each finding, output a JIRA-ready ticket with:
- Title, Type, Severity, Role
- Description & Impact
- Fix Plan: Architecture → Requirements → SDD → OpenAPI → Code → Unit Tests → Verification
- Acceptance Criteria (checklist)
- Estimated Effort (Story Points & person-days)
```

**Review focus:**
- Soundness of the microservices / monolith architecture choice
- Inter-service communication protocols and contracts
- Horizontal scalability and bottleneck identification
- Abstraction layers in the core business logic
- Risk management for third-party dependencies

---

### Role 4 — Security expert: security design & code review

```
Use extended thinking to reason through all findings before responding.

You are a cybersecurity expert conducting a full security review of this
system's design and codebase.

Review:
- Authentication & authorization (RBAC, JWT, OAuth, API keys)
- Input validation, injection risks (SQLi, prompt injection, XSS, etc.)
- Secrets management and data encryption (at rest & in transit)
- API security (rate limiting, abuse prevention, CORS, headers)
- Audit trail completeness and tamper resistance
- Compliance posture (OWASP Top 10, zero-trust principles)

Rank all findings by severity: Critical > High > Medium > Low.

For each finding, output a JIRA-ready ticket with:
- Title, Type, Severity, Role
- Description & Impact
- Fix Plan: Architecture → Requirements → SDD → OpenAPI → Code → Unit Tests → Verification
- Acceptance Criteria (checklist)
- Estimated Effort (Story Points & person-days)
```

**Review focus:**
- JWT / API Key lifecycle management
- Prompt-injection defenses
- Encryption of sensitive data at rest and in transit
- CORS policies and HTTP security header configuration
- Depth of zero-trust principle adoption

---

### Role 5 — Audit expert: compliance & audit-log review

```
Use extended thinking to reason through all findings before responding.

You are an audit and compliance expert reviewing this system's design,
codebase, and operational practices.

Review:
- Completeness of audit logs (who, what, when, where, outcome)
- Log integrity, immutability, and retention policies
- Regulatory compliance readiness (GDPR, SOC2, ISO27001, etc.)
- Data lineage and traceability for AI model calls
- Access control audit trails and privileged action logging
- Incident response and forensic readiness
```

**Review focus:**
- The five elements of audit logs: Who / What / When / Where / Outcome
- Log tamper-resistance mechanisms (immutable storage, hash chains)
- Data lineage for AI model calls
- Independent audit trail for privileged operations
- Automation of compliance report generation

---

### Role 6 — UX/UI expert: interface & interaction review

```
Use extended thinking to reason through all findings before responding.

You are a senior UX/UI designer reviewing this system's interfaces and
functional flows.

Review:
- Visual hierarchy, consistency, and design system adherence
- Information architecture and navigation clarity
- User task flows and cognitive load
- Responsive design and accessibility (WCAG compliance)
- Empty states, error states, and loading states
- Onboarding, tooltips, and user guidance quality

Rank all findings by severity: Critical > High > Medium > Low.

For each finding, output a JIRA-ready ticket with:
- Title, Type, Severity, Role
- Description & Impact
- Fix Plan: Architecture → Requirements → SDD → OpenAPI → Code → Unit Tests → Verification
- Acceptance Criteria (checklist)
- Estimated Effort (Story Points & person-days)
```

**Review focus:**
- Visual hierarchy and design-system consistency
- Step count and cognitive load on critical task flows
- Design of empty / error / loading states
- WCAG 2.1 accessibility compliance
- Responsive adaptation for mobile

---

### Role 7 — Product manager: product strategy & feature completeness

```
Use extended thinking to reason through all findings before responding.

You are an experienced product manager reviewing this system's design,
features, and functional completeness.

Review:
- Feature completeness vs. user needs and market expectations
- Prioritization of features — what's missing vs. over-engineered
- User journey coherence end-to-end
- Metrics, analytics, and feedback loops available in the product
- Monetization and usage tracking capabilities
- Roadmap gaps based on what's built vs. what's needed

Rank all findings by severity: Critical > High > Medium > Low.

For each finding, output a JIRA-ready ticket with:
- Title, Type, Severity, Role
- Description & Impact
- Fix Plan: Architecture → Requirements → SDD → OpenAPI → Code → Unit Tests → Verification
- Acceptance Criteria (checklist)
- Estimated Effort (Story Points & person-days)
```

**Review focus:**
- Fit between the feature set and target-user needs
- Identifying over-engineering vs. missing capabilities
- Coherence of the end-to-end user journey
- Analytics and user-feedback closed loops
- Usage tracking and billing capability

---

### Role 8 — CTO: technical strategy & engineering capability

```
Use extended thinking to reason through all findings before responding.

You are the CTO conducting a holistic review of this system's technical
strategy, architecture, codebase, and operational readiness.

Review:
- Alignment of tech choices with business and scale goals
- Engineering velocity and maintainability of the codebase
- Infrastructure resilience, observability, and disaster recovery
- Build vs. buy decisions and third-party dependencies risk
- Team capability alignment with the tech stack chosen
- Technical roadmap feasibility and risk areas

Rank all findings by severity: Critical > High > Medium > Low.

For each finding, output a JIRA-ready ticket with:
- Title, Type, Severity, Role
- Description & Impact
- Fix Plan: Architecture → Requirements → SDD → OpenAPI → Code → Unit Tests → Verification
- Acceptance Criteria (checklist)
- Estimated Effort (Story Points & person-days)
```

**Review focus:**
- Alignment between tech choices and business-scale goals
- Engineering efficiency: CI/CD maturity, code maintainability
- Infrastructure resilience and observability (Metrics / Tracing / Logging)
- Disaster recovery and RTO/RPO targets
- Risk assessment for build-vs-buy decisions

---

### Role 9 — CIO: business alignment & information governance

```
Use extended thinking to reason through all findings before responding.

You are the CIO reviewing this system from a business, information
governance, and enterprise IT perspective.

Review:
- Alignment of system capabilities with business objectives
- Data governance, ownership, and information lifecycle management
- Integration readiness with enterprise systems (SSO, ERP, SIEM, etc.)
- Vendor lock-in risk and exit strategy for AI model providers
- Total cost of ownership and operational efficiency
- Reporting, dashboards, and executive-level visibility

Rank all findings by severity: Critical > High > Medium > Low.

For each finding, output a JIRA-ready ticket with:
- Title, Type, Severity, Role
- Description & Impact
- Fix Plan: Architecture → Requirements → SDD → OpenAPI → Code → Unit Tests → Verification
- Acceptance Criteria (checklist)
- Estimated Effort (Story Points & person-days)
```

**Review focus:**
- Alignment of system capability with business strategy
- Data ownership, lifecycle, and governance policy
- Integration readiness with enterprise systems (SSO, SIEM, ERP)
- Vendor lock-in risk and exit strategy for AI model providers
- TCO analysis and operational efficiency
- Executive reporting and decision-support visibility

---

## Recommended usage scenarios

| Scenario | Recommended role combination |
|----------|------------------------------|
| **Pre-release full review** | All 9 roles in sequence |
| **Quick iteration review** | Role 3 (Architect) + Role 4 (Security) + Role 6 (UX) |
| **Pre-launch final check** | Role 2 (User) + Role 5 (Audit) + Role 8 (CTO) |
| **Compliance-focused review** | Role 4 (Security) + Role 5 (Audit) + Role 9 (CIO) |
| **Product iteration planning** | Role 2 (User) + Role 6 (UX) + Role 7 (Product) |

---

## Standard implementation workflow (detail)

```
┌────────────────────────────────────────────────────────────────┐
│              Implementation workflow (every role)              │
├────────────────┬───────────────────────────────────────────────┤
│ 1. Architecture │ Decide architecture impact; update diagrams  │
│ 2. Requirements │ Capture functional + non-functional reqs     │
│ 3. SDD          │ Software design doc: modules + data model    │
│ 4. OpenAPI      │ Define or update API spec (OpenAPI 3.x)      │
│ 5. Code         │ Implement per design, follow code standards  │
│ 6. Unit Tests   │ Write unit tests, target coverage >= 80%     │
│ 7. Verification │ Integration + regression tests; confirm fix  │
└────────────────┴───────────────────────────────────────────────┘
```

---

## Appendix: prompt enhancement options

Prepend any of the following to a role prompt as needed:

```
# Enable extended thinking
Use extended thinking to reason through all findings before responding.

# Specify response language
Respond in English.

# Constrain review scope
Focus only on [specific module/feature] in this review.

# Require severity-ordered output
Rank all findings by severity: Critical > High > Medium > Low.

# Require actionable output
For each finding, output a JIRA-ready ticket format with title,
description, acceptance criteria, and estimated effort.
```

---

*Document version: v1.1 | Translated 2026-05-17 (originally 2026-03-27)*
