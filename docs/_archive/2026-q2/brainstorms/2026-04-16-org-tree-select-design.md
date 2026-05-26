# OrgTreeSelect Component Design

**Date:** 2026-04-16
**Scope:** Replace all organization selection UI with a unified tree select component

## Problem

The frontend uses inconsistent patterns for organization selection:
- `FormSelect` + `flattenOrgs()` simulates hierarchy with indentation (OrganizationCreate, ProjectCreate)
- `SearchableCombobox` shows a flat searchable list (QuotaPolicyCreate, QuotaOverrideCreate)
- Raw `Textarea` for manual ID input (RoutingRule)
- Text field for orgId (LiveTrafficFilters)

Organizations are already a tree structure in the backend (`parentId` self-referential relation). The frontend should reflect this with a proper tree select component.

## Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Search | Client-side filtering | Org tree loaded once via `/api/admin/organizations/tree`, filtered locally by name/code. Simpler, instant UX. |
| Display | Tree with expand/collapse | Users already understand the org hierarchy (OrgList page renders it as a tree). Indent + arrows is natural. |
| Multi-select display | Tag/chip in input | Each selected item visible as a removable tag. Intuitive for small selection counts. |
| Cascade | Configurable prop | `cascade` prop (default false) lets callers opt in to parent-child auto-selection. |
| Scope | Replace all 7 org selection points | Unified experience across the entire UI. |

## Component API

```tsx
interface OrgTreeSelectProps {
  /** Selection mode. Default: 'single' */
  mode?: 'single' | 'multiple';

  /** Multi-select: selecting parent auto-selects children. Default: false */
  cascade?: boolean;

  /** Selected org ID(s). string for single, string[] for multiple. */
  value: string | string[];

  /** Fires on selection change. */
  onChange: (value: string | string[]) => void;

  /** Input placeholder text. */
  placeholder?: string;

  /** Disable the component. */
  disabled?: boolean;

  /** Show clear button. Default: true */
  allowClear?: boolean;

  /** CSS class for the root element. */
  className?: string;

  /** Org IDs to exclude from the tree (e.g. exclude self when picking parent). */
  excludeIds?: string[];
}
```

## Data Flow

1. Component mounts -> calls `organizationApi.tree()` once -> caches via `useApi`
2. User types in search box -> local filter matches `name` or `code` (case-insensitive)
3. Matched nodes remain visible; ancestors auto-expand; unmatched branches hide
4. Matched text highlighted in node labels
5. Clear search -> full tree restored

## Dropdown Panel Layout

```
+-------------------------------+
| [Search organizations...]     |  <- search input
+-------------------------------+
| > Root Corp (ROOT)            |  <- expandable parent node
|   v China Region (CN)         |  <- expanded parent
|     Shanghai Office (SH)      |  <- leaf node
|     Beijing Office (BJ)       |
|   > US Region (US)            |  <- collapsed parent
| > Partner Org (PARTNER)       |
+-------------------------------+
```

### Single-select mode
- Click a node -> select it, close dropdown
- Input displays: `Name (code)`

### Multi-select mode
- Each node has a checkbox
- Click toggles checkbox, dropdown stays open
- Input area shows selected items as tags (chips), each with x to remove
- When `cascade=true`: checking parent checks all descendants; unchecking parent unchecks all descendants; all children checked -> parent auto-checks; partial children checked -> parent shows indeterminate state

## Search Behavior

- Filter runs on every keystroke against `node.name` and `node.code` (case-insensitive substring match)
- A node is visible if it matches OR any descendant matches
- Matched nodes: ancestors auto-expanded to reveal them
- Matched substring highlighted in the label
- Empty search -> restore full tree with previous expand/collapse state

## Keyboard Navigation

| Key | Action |
|-----|--------|
| `ArrowDown` | Move focus to next visible node |
| `ArrowUp` | Move focus to previous visible node |
| `ArrowRight` | Expand current node / move to first child if already expanded |
| `ArrowLeft` | Collapse current node / move to parent if already collapsed |
| `Enter` / `Space` | Select node (single) or toggle checkbox (multiple) |
| `Escape` | Close dropdown |
| `Home` | Focus first visible node |
| `End` | Focus last visible node |

## File Structure

```
src/components/ui/OrgTreeSelect/
  OrgTreeSelect.tsx          -- Main component (dropdown trigger, panel, search)
  OrgTreeSelect.module.css   -- All styles
  OrgTreeNode.tsx            -- Single tree node (recursive rendering)
  useOrgTree.ts              -- Data loading, search/filter, expand state, selection logic
  types.ts                   -- TreeNode interface, props types
```

## Replacement Plan

All 7 current org selection points will be replaced:

| # | Page | File | Current | New Config |
|---|------|------|---------|------------|
| 1 | OrganizationCreate | `pages/organizations/OrganizationCreate.tsx` | FormSelect + flattenOrgs | `mode="single" excludeIds=[currentOrgId]` |
| 2 | ProjectCreate | `pages/projects/ProjectCreate.tsx` | FormSelect + flattenOrgs | `mode="single"` |
| 3 | QuotaPolicyCreate | `pages/config/quota-policies/QuotaPolicyCreate.tsx` | SearchableCombobox | `mode="single" allowClear` |
| 4 | QuotaPolicyEdit | `pages/config/quota-policies/QuotaPolicyEdit.tsx` | SearchableCombobox | `mode="single" allowClear` |
| 5 | QuotaOverrideCreate | `pages/config/quota-overrides/QuotaOverrideCreate.tsx` | SearchableCombobox | `mode="single" allowClear` |
| 6 | RoutingRule | `pages/config/routing/MatchConditionExtraFields.tsx` | Textarea (comma-separated IDs) | `mode="multiple"` |
| 7 | LiveTrafficFilters | `pages/analytics/live-traffic/LiveTrafficBasicFilters.tsx` | orgId text field | `mode="single" allowClear` |

## i18n

All user-visible strings use `t()` from react-i18next:
- `common:orgTreeSelect.placeholder` -> en: "Select organization" / es: "Seleccionar organizacion"
- `common:orgTreeSelect.search` -> en: "Search organizations..." / es: "Buscar organizaciones..."
- `common:orgTreeSelect.noMatch` -> en: "No matching organizations" / es: "Sin organizaciones coincidentes"
- `common:orgTreeSelect.clear` -> en: "Clear" / es: "Borrar"
- `common:orgTreeSelect.selected` -> en: "{{count}} selected" / es: "{{count}} seleccionados"
(zh translations live in the zh locale file)

## Accessibility

- Dropdown uses `role="tree"`, nodes use `role="treeitem"`
- `aria-expanded` on expandable nodes
- `aria-selected` on selected nodes
- `aria-checked` on checkboxes in multi-select mode (with `aria-checked="mixed"` for indeterminate)
- `aria-activedescendant` on the search input tracks keyboard focus
- Search input: `role="combobox"` with `aria-controls` pointing to the tree
