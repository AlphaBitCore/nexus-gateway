# Control Plane UI — IAM

The IAM sidebar section manages tenancy and access control. It has eight leaves: **Organizations**, **Projects**, **Users**, **Roles**, **Policies**, **Simulator**, **OAuth Clients**, and **Identity Provider**. Sidebar labels and routes are defined in `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`.

Nexus is the service provider (SP) in any federation; "identity provider" here always means an external IdP that Nexus federates with.

## Organizations

**Purpose.** The top tenant entity in the Organization → Project hierarchy.

**List page.** A tree-style table with columns: name, code, project count, and status. A status filter narrows by enabled or disabled. A row drills into the detail page.

**Create and detail.** Creation collects a name, a code (slug), an optional parent organization (which nests it in the tree), a description, contact name / email / phone, an enabled flag, and a timezone. The detail page has tabs for Info, Members (the admin users in the org, with an include-sub-teams toggle), Projects, and Sub-organizations. An organization cannot be deleted while it still has child organizations or projects.

**Key concepts.** Organizations form a self-referential tree through the parent reference; the hierarchy mirrors the scope segment of an NRN.

**Where the data comes from.** `organizationApi` — `list`, `get`, `create`, `update`, `delete`.

## Projects

**Purpose.** A sub-tenant under an organization — the unit that owns Virtual Keys.

**List page.** Columns: name, code, organization (the parent), virtual-key count, and status.

**Create and detail.** Creation collects a name, a code (slug), the parent organization, a description, and contact name / email. The detail page edits the name, code, description, contacts, and status, and lists the project's Virtual Keys.

**Key concepts.** A project belongs to exactly one organization. Virtual Keys attach at the project level.

**Where the data comes from.** `projectApi` — `list`, `get`, `create`, `update`, `delete`.

## Users

**Purpose.** The human principals of IAM.

**List page.** Columns: display name, email, roles, status, console access, source, organization, and last login. Filters cover status (active or suspended) and console access (yes or no).

**Create and detail.** Creation collects a username, email, password, organization, and the console-access flag. The detail page has three tabs: Info, Permissions, and Devices. The Permissions tab has two sections — the effective policies (each tagged with its source) and the role memberships.

**Key concepts.** A user's `source` is `local` (created in Nexus), `oidc` (federated from an external IdP and provisioned just-in-time on first login), or `scim` (provisioned through SCIM). The `canAccessControlPlane` flag decides whether the user can sign into the Control Plane console: a user with it set is an administrator who can open the admin UI, while a user without it exists only as a principal — it can hold Virtual Keys and be the subject of policies and quotas, but cannot sign into the console. Just-in-time OIDC users are created with console access off by default.

A user's effective permissions come from two sources, shown by a badge on each policy in the Permissions tab: a policy attached **directly** to the user, and a policy attached to a **role** the user is a member of (inherited through that membership).

**Where the data comes from.** `iamApi` — `listUsers`, `getUser`, `createUser`, `updateUser`, `deleteUser`, `getPrincipalPolicies`, `attachPrincipalPolicy`, `detachPrincipalPolicy`.

## Roles

**Purpose.** Named bundles of policies assigned to principals — the role-based layer of access control.

**List page.** Columns: name, description, and actions.

**Create and detail.** The form collects a name, a description, and an optional multi-select of policies to attach (linked right after creation). The detail page manages the role's members and its policy attachments.

**Key concepts.** A role is backed by an IAM group; the UI label is "Role" while the underlying object is the group that policies attach to and users join. A standard set of roles is seeded — among them super-admins, security-admins, provider-admins, viewers, developers, and members — and administrators can create more. Managed policies are seeded alongside them.

**Where the data comes from.** `iamApi` — `listGroups`, `getGroup`, `createGroup`, `updateGroup`, `deleteGroup`, `listGroupMembers`, `addGroupMember`, `removeGroupMember`, `addGroupPolicy`, `removeGroupPolicy`.

## Policies

**Purpose.** The policy documents that grant or deny access — the core of the access-control model.

**List page.** Columns: name, type (managed or custom), description, statement count, and status. Managed policies are read-only — their edit and delete actions are disabled. Filters cover type and enabled.

**Create and detail.** The policy editor authors a document either as visual per-statement cards (each with a statement id, an effect, actions, resources, and a condition) or as raw JSON, validated before saving. The detail page renders the document. A separate principal-policies view lists the policies attached to one principal, each tagged with whether it is attached directly or through a group.

**Key concepts.** A policy document is a version plus a list of statements. Each statement has an optional statement id, an **effect** of `Allow` or `Deny`, a list of actions, a list of resources, and an optional condition. A resource is a Nexus Resource Name (NRN) of the form `nrn:nexus:<service>:<scope>:<resourceType>/<resourceID>` — the service is one of `gateway`, `compliance`, `agent`, `platform`, or `iam`; the scope is an organization id, an `org-id/department` path, or `*`; and any segment may use a `*` wildcard. Conditions express attribute-based rules with operators including `StringEquals`, `StringNotEquals`, `StringLike`, `IpAddress`, `NotIpAddress`, and numeric and date comparisons (`NumericLessThan`, `NumericGreaterThan`, `NumericEquals`, `DateLessThan`, `DateGreaterThan`). Evaluation is explicit-deny first: an explicit `Deny` overrides an `Allow`, and anything not allowed is denied by default. A policy is attached to a role (so its members inherit it) or directly to a user; its `type` is `managed` (seeded and read-only) or `custom`.

**Where the data comes from.** `iamApi` — `listPolicies`, `getPolicy`, `createPolicy`, `updatePolicy`, `deletePolicy`, `getPolicyAttachments`, `getActionCatalog`.

## Simulator

**Purpose.** Dry-run a single authorization decision against the live policy set before relying on it.

**What you see.** An input widget and a result panel.

**Controls.** The inputs are a principal (a type of `api_key`, `virtual_key`, or `nexus_user`, plus a specific id chosen from real principals), an action (from the action catalog), a resource (an NRN, picked by service and resource type), and an optional context. Submitting runs the evaluation.

**Key concepts.** The result is a decision of `Allow` or `Deny`, the list of matched statements (each with its policy id and name, optional statement id, effect, and whether it applied directly or through a group), and a reason. The action and resource pickers are driven by the catalog rather than a hardcoded list.

**Where the data comes from.** `iamApi` — `simulate`, `getActionCatalog`.

## OAuth Clients

**Purpose.** The inventory of third-party applications allowed to authenticate to the platform via OAuth — what `client_id` values the auth server will accept on `/oauth/authorize` and `/oauth/token`. The seeded clients (the Control Plane web console, the agent desktop, the operator-toolkit TUI) live in the same table; this page manages the admin-managed registrations alongside them.

**List page.** Columns: id (monospace), name, type (a pill of either `Public` or `Confidential`), the redirect-URI count (hover for the full list), the allowed-scopes count (hover for the full list), and created date. Row click drills into the detail page; the row's kebab carries Delete only — Edit and Rotate live on the detail page where the active-refresh-token count is in scope.

**Create and detail.** Creation collects an id (kebab-case slug, immutable after create), a name, a type (`public` or `confidential`, also immutable after create), one or more redirect URIs (only `https://`, `http://localhost`, and `http://127.0.0.1` are accepted), a list of allowed scopes (any lowercase scope token; the common ones — `openid`, `profile`, `email`, `offline_access`, `admin`, `traffic:write` — autocomplete), a require-PKCE flag (forced on and disabled for public clients), and access + refresh token TTLs in seconds. The form starts from sensible defaults: `confidential`, PKCE on, `[openid, profile, email]`, 1 hour access TTL, 24 hour refresh TTL.

The detail page is a five-card read-mode layout — Authentication, Redirect URIs, Allowed Scopes, Security, and Activity — with a sticky action header carrying Edit, Rotate secret (confidential only), and Delete. The Authentication card surfaces the client id (with copy) and, for confidential clients, a masked secret with a "last rotated" relative timestamp. The Allowed Scopes card renders each scope as a chip with a plain-English explanation; the `admin` scope shows in a warning tone because it bypasses individual resource grants. The Activity card surfaces the live `activeRefreshTokenCount` — rotating the secret does **not** revoke those tokens, so the rotate-confirm dialog repeats that warning with the live count interpolated in.

**Secret reveal.** For confidential clients, the plaintext `client_secret` is returned exactly once — on create and on each rotate-secret call. The reveal modal is hard-gated: an explicit "I have copied and stored this secret securely" checkbox must be ticked before the Close button enables; the secret is never echoed in any other response, audit row, or log line, and cannot be recovered later. Losing the plaintext forces a rotation.

**Delete cascade.** Deleting an OAuth client cascades to its `RefreshToken` rows via the FK `onDelete: Cascade`, immediately invalidating any session that was issued under that registration. The delete-confirm dialog is type-to-confirm — the admin must type the client id exactly — and surfaces the live active-refresh-token count so the blast radius is visible before commit.

**Key concepts.** Two types: **Public** clients (browser SPAs, native CLIs) authenticate by PKCE alone and the token endpoint rejects any presented `client_secret` for them. **Confidential** clients carry a `client_secret` and authenticate to `/oauth/token` via the RFC 6749 §2.3.1 `Authorization: Basic` header (or the `client_secret` form fallback). Rotating a secret invalidates the previous secret immediately but does not revoke outstanding refresh tokens; consumers continue to grant access until the refresh TTL elapses. The IAM resource is `oauth-client`, with `read` / `create` / `update` / `delete` / `rotate` verbs — UI affordances are gated against the matching verbs so a holder of `oauth-client.read` sees the list and detail but no Edit / Rotate / Delete buttons.

**Where the data comes from.** `oauthClientApi` — `list`, `getOne`, `create`, `update`, `rotateSecret`, `remove`. The `getOne` response embeds `activeRefreshTokenCount` so the detail page renders the Activity card in a single round-trip; the `list` response omits the aggregate to keep the page fast.

## Identity Provider

**Purpose.** Manage the external identity providers Nexus federates with for single sign-on.

**List page.** A list of identity providers; the local fallback provider is hidden from the UI.

**Create and detail.** The form selects a protocol — OIDC or SAML. An OIDC provider collects a name, an enabled flag, the issuer, client id, client secret (masked on edit), redirect URI, JWKS URI, authorize URL, token URL, scopes (defaulting to `openid profile email`), an email claim, and a group claim. A SAML provider collects a name, an enabled flag, an entity id, an SSO URL, and a certificate. A test action probes the candidate or saved configuration. On the detail page, two further features attach to a provider: mapping an external IdP group to a role, and managing the provider's SCIM tokens.

**Key concepts.** The protocol is `oidc` or `saml`. The group claim and the external-group-to-role mapping are what let SSO group membership drive Nexus role membership.

**Where the data comes from.** `iamApi` — `listIdentityProviders`, `getIdentityProvider`, `createIdentityProvider`, `updateIdentityProvider`, `deleteIdentityProvider`, `testCandidateIdentityProvider`, `testSavedIdentityProvider`, `listScimTokens`, `createScimToken`, `revokeScimToken`, `listIdpGroupMappings`, `createIdpGroupMapping`, `deleteIdpGroupMapping`.

## References

- `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` — route registry and `nav: { sectionKey: 'iam', ... }` blocks
- `packages/control-plane-ui/src/i18n/locales/en/nav.json` — sidebar labels
- `packages/control-plane-ui/src/pages/iam/organizations/` — Organizations list, create, detail
- `packages/control-plane-ui/src/pages/iam/projects/` — Projects list, create, detail
- `packages/control-plane-ui/src/pages/iam/users/` and `packages/control-plane-ui/src/pages/iam/user-detail/` — Users list and detail tabs (Info, Permissions, Devices)
- `packages/control-plane-ui/src/pages/iam/roles/` — Roles (IAM groups) list, form, detail
- `packages/control-plane-ui/src/pages/iam/oauth-clients/` — OAuth Clients list, detail (5 cards), form (create/edit), and the three confirm dialogs
- `packages/control-plane-ui/src/pages/iam/policies/` — Policy list, editor, detail, and principal-policies view
- `packages/control-plane-ui/src/pages/iam/simulator/IamSimulator.tsx` — Simulator
- `packages/control-plane-ui/src/pages/devices/auth/` — Identity Provider pages (routed under `/iam/identity-providers`)
- `packages/control-plane/internal/identity/iam/nrn.go` — NRN construction
- `packages/control-plane/internal/identity/iam/engine.go` — policy evaluation (explicit-deny-first)
- `packages/control-plane/internal/identity/authserver/store/federated_store.go` — JIT OIDC user provisioning
- `packages/control-plane-ui/src/api/` — `iamApi`, `organizationApi`, `projectApi`
- `tools/db-migrate/schema.prisma` — `Organization`, `Project`, `NexusUser`, `IamGroup`, `IamPolicy`, `IamPolicyAttachment`, `IamGroupMembership`, `IamGroupPolicyAttachment` models
