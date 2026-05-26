# Setup Wizard Redesign — Step-by-Step with Auto-Detection

**Date:** 2026-04-16
**Scope:** Refactor /setup into a linear wizard with real data detection, replacing the manual checklist

## Problem

The current setup wizard is a flat checklist with manual "Mark complete" buttons. Steps don't verify actual system state — operators can mark steps done without creating anything, or miss that seed data already satisfies a step. The step order also misses Organization and Project creation, and doesn't bind Credentials to Providers.

## Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Interaction | Linear wizard (one step at a time) | Steps have dependencies (org → project → VK); linear flow matches naturally |
| Completion detection | Auto-detect via API queries | Real data is the source of truth; no manual marking needed |
| Persistence | No server-side setup-state needed | Each step queries live data on mount; seed data auto-detected |
| Completion view | Summary page with data counts | Gives admin a quick config overview; also serves as re-check entry point |

## Steps

| # | Name | Required | Detection API | Complete When |
|---|------|----------|--------------|---------------|
| 1 | System Health Check | Yes | `GET /api/admin/status` | Core components healthy (DB, Redis) |
| 2 | Create Organization | Yes | `GET /api/admin/organizations` | `data.length > 0` |
| 3 | Connect AI Provider | Yes | `GET /api/admin/providers` + `GET /api/admin/credentials` | >= 1 enabled provider with >= 1 credential |
| 4 | Create Project | Yes | `GET /api/admin/projects` | `data.length > 0` |
| 5 | Issue Virtual Key | Yes | `GET /api/admin/virtual-keys` | `data.length > 0` |
| 6 | Define Routing Rule | Yes | `GET /api/admin/routing-rules` | `data.length > 0` |
| 7 | Configure Compliance | No | None (manual) | User clicks Skip or Done |

## Wizard Layout

Top: horizontal step bar showing all steps with status indicators (checkmark = complete, filled circle = current, empty circle = upcoming).

Main area: single active step content.

Bottom: Back / Next navigation buttons. Next is disabled only on Step 1 if health check fails critically.

## Step Behaviors

### Step 1 — System Health Check
- Calls status API on mount
- Displays component health table (DB, Redis, Gateway)
- All healthy → green summary, auto-complete
- Unhealthy → red warning with details, Next still enabled (non-blocking)

### Step 2 — Create Organization
- Queries `GET /api/admin/organizations`
- Has data → summary table (name, code, child count)
- No data → inline form: name (required), code (required), description (optional)
- On create success → re-query, show summary

### Step 3 — Connect AI Provider
- Queries providers and credentials
- Has enabled provider with credential → summary (provider name, type, credential count)
- No data → two-part form:
  1. Provider: template dropdown (OpenAI, Anthropic, etc.) or custom (name + baseUrl)
  2. Credential: API key input, bound to the provider just created
- Creates both in sequence; on success → re-query, show summary

### Step 4 — Create Project
- Queries `GET /api/admin/projects`
- Has data → summary (name, code, organization name)
- No data → inline form: name (required), code (required), organizationId (OrgTreeSelect)
- On create success → re-query, show summary

### Step 5 — Issue Virtual Key
- Queries `GET /api/admin/virtual-keys`
- Has data → summary (slug, project name if any)
- No data → inline form: slug (required), projectId (optional select)
- On create success → re-query, show summary

### Step 6 — Define Routing Rule
- Queries `GET /api/admin/routing-rules`
- Has data → summary (rule name, strategy type, priority)
- No data → simplified form: name (default "Default Route"), auto single-provider strategy, select provider + model
- On create success → re-query, show summary

### Step 7 — Configure Compliance (Optional)
- No auto-detection
- Shows 3 sub-items with descriptions and "Open" links:
  - Hooks (/config/hooks)
  - Quota Policies (/config/quota-policies)
  - Compliance Proxy (/proxy/status)
- Two buttons: "Skip" (proceed to summary) and "Done" (proceed to summary)

### Summary Page (after all steps)
- Shows config overview table: Organizations (count + names), Providers (count + names), Credentials (count), Projects (count + names), Virtual Keys (count), Routing Rules (count + names), Compliance status
- Buttons: "Go to Dashboard" (navigate to /), "Restart Wizard" (go back to step 1)
- Accessing /setup after completion shows this summary page

## File Structure

```
src/pages/setup/
  SetupWizardPage.tsx          -- Rewrite: linear wizard container + step bar + nav
  SetupWizardPage.module.css   -- Rewrite: step bar + wizard layout styles
  steps/
    StepHealthCheck.tsx        -- Step 1: status API check
    StepOrganization.tsx       -- Step 2: org detection + create form
    StepProvider.tsx           -- Step 3: provider + credential detection + create form
    StepProject.tsx            -- Step 4: project detection + create form
    StepVirtualKey.tsx         -- Step 5: VK detection + create form
    StepRoutingRule.tsx        -- Step 6: routing rule detection + create form
    StepCompliance.tsx         -- Step 7: optional compliance links
    StepSummary.tsx            -- Completion summary page
  useSetupWizard.ts            -- Step state management + auto-detection orchestration
```

### useSetupWizard Hook

Manages:
- `currentStep: number` — active step index (0-7, where 7 = summary)
- `stepResults: StepResult[]` — per-step detection result (loading / complete / incomplete + data)
- On mount: parallel-fetch all detection APIs, determine first incomplete step, set as current
- `goNext()` / `goBack()` — navigation
- `goToStep(n)` — jump to completed or current step (cannot jump ahead of current)
- `refreshStep(n)` — re-run detection for step n (called after inline form create)
- No server-side persistence — all state derived from live API data

### StepResult Shape

```typescript
interface StepResult {
  status: 'loading' | 'complete' | 'incomplete' | 'error';
  data?: unknown;  // step-specific summary data
  error?: string;
}
```

## Removed

- `setup-state` server-side persistence (no longer needed)
- `SetupBanner` component referencing setup-state API
- Manual "Mark complete" / "Mark incomplete" buttons
- `WizardProviderForm.tsx`, `WizardRoutingForm.tsx`, `WizardVirtualKeyForm.tsx` (absorbed into new step components)

## i18n

All user-visible text uses `t('pages:setup.xxx')`. New keys needed for:
- Step titles and descriptions (7 steps + summary)
- Form labels and placeholders
- Summary page labels
- Navigation buttons (Back, Next, Skip, Done, Go to Dashboard, Restart Wizard)
- Status messages (loading, complete, error)

Add to all 3 locales (en/zh/es) in both src and public.

## Accessibility

- Step bar uses `role="tablist"` with `role="tab"` for each step
- Active step content uses `role="tabpanel"`
- Keyboard: Tab through step bar, Enter to activate, arrow keys between steps
- Focus moves to step content when navigating
