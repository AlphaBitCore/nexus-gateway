# Control Plane UI - Prime Console Navigation Alignment

Design tokens are bridged through `ui-shared` via `prime-shadcn-tokens.css`
plus `light.css` / `dark.css`. Page-level alignment should progress
incrementally in the same order as `buildSidebarNavSections()`.

## Phase A - Shared App Shell

| Item | Status | Notes |
|------|--------|-------|
| `PageHeader` | Done | 28px bold title treatment with muted supporting copy, aligned with Prime / THALEON. |
| `Shell` / `Sidebar` | Done | Structure aligns with Prime and uses the current token set for spacing and weight. |
| List and form baseline | Done | Uses `ListFilterToolbar`, shadcn `Button`, and `DataTable` as the baseline. |

## Phase B - Navigation Sections

### overview

1. `/` - Dashboard
2. `/traffic` - Live traffic
3. `/analytics` - Analytics
4. `/quota-usage` - Quota usage
5. `/cache-roi` - Cache ROI

### aiGateway

6. `/ai-gateway/providers`
7. `/ai-gateway/credentials`
8. `/ai-gateway/credential-reliability`
9. `/ai-gateway/routing`
10. `/ai-gateway/virtual-keys`
11. `/ai-gateway/quota-policies`
12. `/ai-gateway/quota-overrides`
13. `/ai-gateway/cache`
14. `/ai-gateway/passthrough`

### compliance

15. `/compliance/overview`
16. `/compliance/hooks`
17. `/compliance/rule-packs`
18. `/compliance/interception-domains`
19. `/compliance/exemptions`
20. `/compliance/ai-guard`
21. `/compliance/streaming`
22. `/compliance/payload-capture`
23. `/compliance/audit-logs`
24. `/compliance/dsar`

### devices

25. `/devices`
26. `/devices/groups`
27. `/devices/device-auth`
28. `/devices/device-defaults`

### alerts

29. `/alerts`
30. `/alerts/rules`
31. `/alerts/channels`

### infrastructure

32. `/infrastructure/nodes`
33. `/infrastructure/config-sync`
34. `/infrastructure/overrides`
35. `/infrastructure/jobs`
36. `/infrastructure/errors`
37. `/infrastructure/crashes`
38. `/infrastructure/diag-mode`
39. `/infrastructure/observability-config`
40. `/infrastructure/observability-retention`
41. `/infrastructure/siem`
42. `/infrastructure/proxy-rollout`
43. `/infrastructure/agent-setup`
44. `/infrastructure/kill-switch`

### iam

45. `/iam/organizations`
46. `/iam/projects`
47. `/iam/users`
48. `/iam/roles`
49. `/iam/policies`
50. `/iam/simulator`
51. `/iam/identity-providers`

### system

52. `/tools/ai-gateway-simulator`
53. `/status`
54. `/setup`

## Page Acceptance Notes

- Header: use `PageHeader` or an equivalent Tailwind hierarchy; avoid a new bespoke `h1` size.
- Body: prefer Tailwind semantic colors such as `text-foreground` and `text-muted-foreground`, or existing Prime / THALEON tokens.
- List pages: keep title, toolbar, and table spacing aligned with already-migrated pages such as Traffic.

## Maintenance

When adding a `SHELL_ROUTES` entry with `nav` metadata, append its route to the
matching Phase B section so design reviews can track alignment coverage.
