# Cross-feature end-to-end flows

> Each doc walks one end-to-end flow that touches multiple services and surfaces. For the architectural sequence diagrams underpinning these, see `docs/developers/architecture/cross-cutting/foundation/multi-endpoint-coordination-architecture.md`. These feature docs are the **user/operator-facing** narrative.

## Index

| Flow | When you read this |
|---|---|
| [Virtual Key lifecycle](vk-lifecycle.md) | An application needs API access |
| [Routing rule lifecycle](routing-rule-lifecycle.md) | Steering traffic across providers / models |
| [Hook rollout](hook-rollout.md) | Enabling a new compliance hook safely |
| [Kill switch & emergency passthrough](kill-switch-and-passthrough.md) | The compliance pipeline can't run; we need to keep traffic moving |
| [Agent enrollment](agent-enrollment.md) | Putting Nexus on a workstation |
| [Credential rotation](credential-rotation.md) | Rotating a provider API key with zero downtime |
| [IdP federation login](idp-federation.md) | A user signs in via Okta / Azure AD / OIDC / SAML |
| [Traffic event lifecycle](traffic-event-lifecycle.md) | A `/v1/*` request hits Nexus, audits flow back |
| [Alert evaluation](alert-evaluation.md) | A metric crosses threshold, the right humans get paged |

## Conventions

Each flow doc follows this shape:

1. **What this flow accomplishes** — one sentence.
2. **Actors** — admin / app / user / agent / Hub / data-plane Thing.
3. **Sequence** — numbered steps; cross-link to the relevant arch doc for "why".
4. **Failure modes** — where the flow can stall and what the operator does about it.
5. **Verification** — how to confirm "yes, this happened correctly".
6. **References** — arch doc + relevant CP-UI / agent-UI section + relevant skill.
