/**
 * CatalogPicker — three-level catalog browser for the IAM Policy Editor.
 *
 * Hierarchy:
 *
 *   Service (gateway / compliance / agent / platform / iam)
 *     └── Resource (provider, model, hook, ...)
 *           └── Verb (create, read, update, ...)
 *
 * Each level has a master checkbox:
 *   • Top-level "admin:*" wildcard — every catalog action everywhere.
 *   • Service master — selects/clears every admin:<resource>.* wildcard
 *     for the resources in that service.
 *   • Resource master — selects/clears the admin:<resource>.* wildcard
 *     (or every individual verb, whichever is more compact).
 *   • Per-verb checkbox toggles admin:<resource>.<verb> directly.
 *
 * Filter input matches against service name, resource name, or action
 * name; the matching rows auto-expand so the filtered set is visible.
 */
import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import type { ActionCatalogEntry, ActionCatalogResponse } from '@/api/services';
import { Input } from '@/components/ui';
import styles from '../_shared/CatalogPicker.module.css';

interface CatalogPickerProps {
  catalog: ActionCatalogResponse | null;
  /** Currently-selected actions (parsed from the statement's chip list). */
  currentActions: string[];
  /** Emits the full next action list. */
  onChange: (next: string[]) => void;
  /** Optional close handler — renders a × button in the header. */
  onClose?: () => void;
}

type SelectionState = 'none' | 'wildcard' | 'all-specific' | 'partial';

/**
 * The 5 canonical service buckets — order matters for human reading.
 * Mirrors the order in shared/iam/catalog_data.go.
 */
const SERVICE_ORDER: readonly string[] = [
  'gateway',
  'compliance',
  'agent',
  'platform',
  'iam',
] as const;

function resourceSelectionState(
  type: string,
  actions: ActionCatalogEntry['actions'],
  current: string[],
): SelectionState {
  const wildcard = `admin:${type}.*`;
  if (current.includes(wildcard)) return 'wildcard';
  if (actions.length === 0) return 'none';
  const selected = actions.filter((a) => current.includes(a.name));
  if (selected.length === 0) return 'none';
  if (selected.length === actions.length) return 'all-specific';
  return 'partial';
}

/**
 * Aggregate the selection state across every resource in a service.
 * 'wildcard' = every resource has its wildcard selected;
 * 'partial'  = at least one resource has some selection;
 * 'none'     = no resource in this service has any selection.
 * (We collapse 'all-specific' into 'wildcard' here because at the
 * service level a service is "fully selected" either way.)
 */
function serviceSelectionState(
  resources: ActionCatalogEntry[],
  current: string[],
): SelectionState {
  if (resources.length === 0) return 'none';
  let fully = 0;
  let any = 0;
  for (const r of resources) {
    const s = resourceSelectionState(r.type, r.actions, current);
    if (s === 'wildcard' || s === 'all-specific') fully += 1;
    if (s !== 'none') any += 1;
  }
  if (fully === resources.length) return 'wildcard';
  if (any === 0) return 'none';
  return 'partial';
}

export function CatalogPicker({
  catalog,
  currentActions,
  onChange,
  onClose,
}: CatalogPickerProps) {
  const { t } = useTranslation();
  const [filter, setFilter] = useState('');
  const [openGroups, setOpenGroups] = useState<Set<string>>(new Set());
  const [openServices, setOpenServices] = useState<Set<string>>(new Set());

  const hasAdminWildcard = currentActions.includes('admin:*');

  const toggleGroup = (type: string) => {
    setOpenGroups((prev) => {
      const next = new Set(prev);
      if (next.has(type)) next.delete(type);
      else next.add(type);
      return next;
    });
  };

  const toggleService = (service: string) => {
    setOpenServices((prev) => {
      const next = new Set(prev);
      if (next.has(service)) next.delete(service);
      else next.add(service);
      return next;
    });
  };

  const setAdminWildcard = (on: boolean) => {
    if (on) {
      // admin:* subsumes everything; drop other admin: entries to
      // avoid redundancy.
      const nonAdmin = currentActions.filter((a) => !a.startsWith('admin:'));
      onChange([...nonAdmin, 'admin:*']);
    } else {
      onChange(currentActions.filter((a) => a !== 'admin:*'));
    }
  };

  const toggleResourceMaster = (resource: ActionCatalogEntry) => {
    const state = resourceSelectionState(resource.type, resource.actions, currentActions);
    const wildcard = `admin:${resource.type}.*`;
    const verbNames = resource.actions.map((a) => a.name);

    if (state === 'wildcard' || state === 'all-specific') {
      // Currently fully selected → unselect everything for this resource.
      const filtered = currentActions.filter(
        (a) => a !== wildcard && !verbNames.includes(a),
      );
      onChange(filtered);
    } else {
      // Either partial or none → select all via wildcard (compactness).
      const cleared = currentActions.filter(
        (a) => a !== wildcard && !verbNames.includes(a),
      );
      onChange([...cleared, wildcard]);
    }
  };

  const toggleServiceMaster = (_service: string, resources: ActionCatalogEntry[]) => {
    const state = serviceSelectionState(resources, currentActions);
    // Build the set of strings this service's wildcards + verbs use.
    const wildcards = resources.map((r) => `admin:${r.type}.*`);
    const allVerbNames = resources.flatMap((r) => r.actions.map((a) => a.name));

    if (state === 'wildcard') {
      // Fully selected — clear every wildcard and verb for this service.
      const filtered = currentActions.filter(
        (a) => !wildcards.includes(a) && !allVerbNames.includes(a),
      );
      onChange(filtered);
    } else {
      // None or partial — add every resource's wildcard, removing any
      // specific verbs that would now be subsumed.
      const cleared = currentActions.filter(
        (a) => !wildcards.includes(a) && !allVerbNames.includes(a),
      );
      onChange([...cleared, ...wildcards]);
    }
  };

  const toggleVerb = (resource: ActionCatalogEntry, actionName: string) => {
    const wildcard = `admin:${resource.type}.*`;
    if (currentActions.includes(actionName)) {
      onChange(currentActions.filter((a) => a !== actionName));
      return;
    }
    if (currentActions.includes(wildcard)) {
      const expanded = resource.actions.map((a) => a.name);
      const without = currentActions.filter((a) => a !== wildcard);
      onChange([...without, ...expanded.filter((a) => a !== actionName)]);
      return;
    }
    onChange([...currentActions, actionName]);
  };

  // Group resources by service, honouring SERVICE_ORDER for display.
  const grouped = useMemo(() => {
    const q = filter.trim().toLowerCase();
    const buckets = new Map<string, ActionCatalogEntry[]>();
    for (const r of catalog?.resources ?? []) {
      const matches =
        !q ||
        r.service.toLowerCase().includes(q) ||
        r.type.toLowerCase().includes(q) ||
        r.actions.some((a) => a.name.toLowerCase().includes(q));
      if (!matches) continue;
      if (!buckets.has(r.service)) buckets.set(r.service, []);
      buckets.get(r.service)!.push(r);
    }
    // Sort resources within each service alphabetically.
    for (const list of buckets.values()) {
      list.sort((a, b) => a.type.localeCompare(b.type));
    }
    // Walk SERVICE_ORDER for the canonical display order; surface any
    // service we don't recognise at the end (defensive — the catalog
    // should never emit a service outside this list).
    const ordered: Array<{ service: string; resources: ActionCatalogEntry[] }> = [];
    for (const s of SERVICE_ORDER) {
      const r = buckets.get(s);
      if (r && r.length > 0) ordered.push({ service: s, resources: r });
    }
    for (const [s, r] of buckets.entries()) {
      if (SERVICE_ORDER.includes(s)) continue;
      ordered.push({ service: s, resources: r });
    }
    return ordered;
  }, [catalog, filter]);

  return (
    <div className={styles.picker} role="region" aria-label={t('pages:iam.catalogPickerAria')}>
      <div className={styles.header}>
        <Input
          type="search"
          className={styles.filterInput}
          placeholder={t('pages:iam.catalogPickerFilterPlaceholder')}
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          aria-label={t('pages:iam.catalogPickerFilterAria')}
        />
        {onClose && (
          <button
            type="button"
            className={styles.closeBtn}
            onClick={onClose}
            aria-label={t('pages:iam.catalogPickerClose')}
          >
            ×
          </button>
        )}
      </div>

      <label className={hasAdminWildcard ? styles.masterRowActive : styles.masterRow}>
        <input
          type="checkbox"
          checked={hasAdminWildcard}
          onChange={(e) => setAdminWildcard(e.target.checked)}
        />
        <strong>{t('pages:iam.catalogPickerAdminWildcard')}</strong>
        <code className={styles.masterRowCode}>admin:*</code>
      </label>

      <div className={styles.resourceList}>
        {grouped.length === 0 && (
          <div className={styles.emptyState}>{t('pages:iam.catalogPickerNoMatch')}</div>
        )}
        {grouped.map(({ service, resources }) => {
          const svcState = serviceSelectionState(resources, currentActions);
          const svcOpen =
            openServices.has(service) || filter.trim().length > 0;
          const svcChecked = svcState === 'wildcard';
          const svcIndeterminate = svcState === 'partial';
          const svcSelectedResources = resources.filter((r) => {
            const s = resourceSelectionState(r.type, r.actions, currentActions);
            return s !== 'none';
          }).length;
          return (
            <div key={service} className={styles.serviceGroup}>
              <div className={styles.serviceHeader}>
                <button
                  type="button"
                  className={svcOpen ? styles.chevronOpen : styles.chevron}
                  onClick={() => toggleService(service)}
                  aria-expanded={svcOpen}
                  aria-label={t(
                    svcOpen
                      ? 'pages:iam.catalogPickerCollapseService'
                      : 'pages:iam.catalogPickerExpandService',
                  )}
                >
                  ▶
                </button>
                <label className={styles.serviceMasterLabel}>
                  <input
                    type="checkbox"
                    checked={svcChecked}
                    ref={(el) => {
                      if (el) el.indeterminate = svcIndeterminate;
                    }}
                    onChange={() => toggleServiceMaster(service, resources)}
                  />
                  <span className={styles.serviceName}>
                    {t(`pages:iam.services.${service}`, { defaultValue: service })}
                  </span>
                  <span className={styles.serviceCount}>
                    {svcSelectedResources > 0
                      ? t('pages:iam.catalogPickerServiceCount', {
                          selected: svcSelectedResources,
                          total: resources.length,
                        })
                      : t('pages:iam.catalogPickerResourceCount', { count: resources.length })}
                  </span>
                </label>
              </div>

              {svcOpen && (
                <div className={styles.serviceBody}>
                  {resources.map((r) => {
                    const state = resourceSelectionState(r.type, r.actions, currentActions);
                    const open = openGroups.has(r.type) || filter.trim().length > 0;
                    const selectedCount =
                      state === 'wildcard' ? r.actions.length
                        : r.actions.filter((a) => currentActions.includes(a.name)).length;
                    const masterChecked = state === 'wildcard' || state === 'all-specific';
                    const masterIndeterminate = state === 'partial';
                    return (
                      <div key={r.type} className={styles.resourceGroup}>
                        <div className={styles.resourceHeader}>
                          <button
                            type="button"
                            className={open ? styles.chevronOpen : styles.chevron}
                            onClick={() => toggleGroup(r.type)}
                            aria-expanded={open}
                            aria-label={t(
                              open
                                ? 'pages:iam.catalogPickerCollapseGroup'
                                : 'pages:iam.catalogPickerExpandGroup',
                            )}
                          >
                            ▶
                          </button>
                          <label className={styles.resourceMasterLabel}>
                            <input
                              type="checkbox"
                              checked={masterChecked}
                              ref={(el) => {
                                if (el) el.indeterminate = masterIndeterminate;
                              }}
                              onChange={() => toggleResourceMaster(r)}
                            />
                            <span className={styles.resourceName}>{r.type}</span>
                            <span className={styles.resourceCount}>
                              {selectedCount > 0
                                ? t('pages:iam.catalogPickerSelectedCount', {
                                    selected: selectedCount,
                                    total: r.actions.length,
                                  })
                                : t('pages:iam.catalogPickerVerbCount', { count: r.actions.length })}
                            </span>
                          </label>
                        </div>
                        {open && (
                          <div className={styles.verbList}>
                            {r.actions.map((a) => {
                              const checked =
                                currentActions.includes(a.name) ||
                                currentActions.includes(`admin:${r.type}.*`);
                              return (
                                <label key={a.name} className={styles.verbRow}>
                                  <input
                                    type="checkbox"
                                    checked={checked}
                                    onChange={() => toggleVerb(r, a.name)}
                                  />
                                  <code className={styles.verbName}>{a.name}</code>
                                </label>
                              );
                            })}
                          </div>
                        )}
                      </div>
                    );
                  })}
                </div>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}
