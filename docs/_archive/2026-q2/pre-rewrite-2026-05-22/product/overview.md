# Nexus Gateway -- Product Overview

## What Is Nexus Gateway

Nexus Gateway is an enterprise AI traffic gateway that provides centralized governance, compliance enforcement, intelligent routing, and observability for all AI API traffic within an organization. It sits between enterprise applications and AI providers (OpenAI, Anthropic, Azure OpenAI, Google Gemini, DeepSeek, GLM, MiniMax), enforcing security policies, auditing every interaction, and giving IT and compliance teams full visibility and control over AI usage.

Nexus Gateway is not a single proxy -- it is a platform with multiple deployment modes that cover every way AI APIs are consumed in the enterprise: SDK integrations, network-level interception, and endpoint-level desktop agents. All deployment modes are coordinated by a central platform tier (Nexus Hub) that handles registration, configuration sync, alerting, and audit aggregation across the fleet.

## Value Proposition

- **Centralized AI governance**: One control plane for all AI traffic policies, credentials, and audit logs across the organization.
- **Compliance enforcement at the network layer**: PII detection, keyword filtering, content safety checks, IP access control, and rate limiting applied consistently to every AI request -- regardless of which application or developer initiated it.
- **Credential isolation**: Developers never see raw provider API keys. Virtual keys with scoped permissions replace direct provider credentials.
- **Full audit trail**: Every AI request and response is logged with data classification labels, hook decisions, routing traces, and provider metrics.
- **Provider-agnostic routing**: Route requests across multiple AI providers with fallback chains, weighted load balancing, A/B splits, and conditional logic -- all configured through the dashboard without code changes.

## Target Audience

- **Enterprise IT and security teams** managing AI adoption at scale
- **Compliance officers** enforcing data handling policies (PII, confidential data classification)
- **Platform engineering teams** building internal AI platforms with centralized credential and quota management
- **CISOs and risk managers** requiring audit trails and kill-switch capabilities for AI traffic

## Trinity Consistency

Nexus Gateway offers three deployment modes that all execute the same Go compliance code from the shared `packages/shared` library:

| Mode | Service | How It Works |
|------|---------|-------------|
| **AI Gateway** (SDK Proxy) | `ai-gateway` | Applications send AI API calls to the gateway's `/v1/*` endpoints using virtual keys. The gateway authenticates, runs compliance hooks, routes to the optimal provider, and returns the response. |
| **Compliance Proxy** (Transparent Proxy) | `compliance-proxy` | A TLS-intercepting HTTPS proxy that transparently inspects AI traffic at the network level. Applications use it as their HTTPS proxy -- no SDK changes required. Performs TLS bump, content extraction, and compliance enforcement. |
| **Desktop Agent** | `agent` | A lightweight agent installed on developer workstations. **macOS is the production-validated platform today**; Linux (iptables) and Windows (WinDivert) backends are scaffolds in development (see `docs/developers/architecture/services/agent/agent-forwarder-architecture.md` §"Platform support matrix" lines 41–46). Intercepts outbound AI traffic at the OS level, enforces policies locally, and syncs audit events back to the control plane. |

All three modes share the same hook implementations (PII detection, keyword filtering, content safety, rate limiting, request size validation, IP access filtering), the same data classification taxonomy (Public, Internal, Confidential, Restricted), and the same compliance pipeline logic. A policy configured in the dashboard applies consistently regardless of how the traffic reaches the gateway.

## Capabilities Summary

- Compliance hook pipeline (request gate, response gate, connection-level checks, streaming compliance modes)
- PII detection and data classification
- Keyword filtering and content safety enforcement
- Rate limiting (distributed via Redis, local fallback)
- Intelligent routing engine (single, fallback, load balance, conditional, A/B split, policy narrowing) with smart routing dispatch
- Two-layer prompt-cache savings (provider-side cached content via opt-in marker injection for Anthropic/Bedrock/Gemini, plus the Nexus exact-match response cache)
- Response cache and quota engine with per-organization / per-VK budget enforcement
- Virtual key authentication with per-key model restrictions
- Credential vault with AES-256-GCM encryption and rotation propagation
- Multi-provider support (OpenAI, Anthropic, Azure OpenAI, Gemini, DeepSeek, GLM, MiniMax, and 40+ more via the adapter framework)
- Real-time traffic monitoring and analytics dashboard
- Audit logging with SIEM forwarding and S3 spillstore for full prompt+response bodies
- Fleet management for desktop agents (enrollment via Hub CA, node-based config sync, out-of-sync detection)
- IAM with policy-based access control (RBAC/ABAC, NRN resource model)
- External IdP federation (OIDC today; SAML planned — see roadmap E87) with JIT user provisioning
- Organization hierarchy and project scoping
- Alerting with configurable thresholds and notification channels
- Three-tier kill switch and emergency passthrough fallback (E48) for compliance-pipeline outages
- Auto-update mechanism for desktop agents
