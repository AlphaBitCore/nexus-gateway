/**
 * ScopedActionsPicker — single-resource verb picker inspired by AWS IAM
 * Visual Editor's "Actions allowed" panel.
 *
 * Used when a statement's actions are all confined to one catalog
 * resource (the common case). The picker mirrors AWS's interaction:
 *
 *   • Filter input narrows verbs as the user types.
 *   • A master checkbox toggles `admin:<resource>.*` — wildcard, picks
 *     up every current and future verb on the resource.
 *   • Individual verb checkboxes toggle the specific
 *     `admin:<resource>.<verb>` action.
 *
 * The component is dumb wrt mode switching: it just diffs the supplied
 * `currentActions` against the catalog's verbs for one resource and
 * emits onChange with the new full action list. The parent (IAM Policy
 * Editor) is responsible for deciding when to render this vs. the
 * mixed-mode chip input.
 */
import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import type { ActionCatalogEntry } from '@/api/services';
import { Input } from '@/components/ui';
import styles from '../_shared/ScopedActionsPicker.module.css';

interface ScopedActionsPickerProps {
  /** The catalog row describing the resource (verbs + NRN). */
  resource: ActionCatalogEntry;
  /** Currently-selected actions (parsed from the chip list / textarea). */
  currentActions: string[];
  /** Emits the full next action list. */
  onChange: (next: string[]) => void;
}

export function ScopedActionsPicker({
  resource,
  currentActions,
  onChange,
}: ScopedActionsPickerProps) {
  const { t } = useTranslation();
  const [filter, setFilter] = useState('');

  const wildcard = `admin:${resource.type}.*`;
  const verbActions = resource.actions; // [{ verb, name, siem }, ...]

  const hasWildcard = currentActions.includes(wildcard);
  const allSpecificSelected =
    !hasWildcard &&
    verbActions.length > 0 &&
    verbActions.every((a) => currentActions.includes(a.name));
  const masterChecked = hasWildcard || allSpecificSelected;
  const selectedCount =
    hasWildcard
      ? verbActions.length
      : verbActions.filter((a) => currentActions.includes(a.name)).length;
  const masterIndeterminate =
    !masterChecked && selectedCount > 0;

  const filtered = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return verbActions;
    return verbActions.filter(
      (a) => a.name.toLowerCase().includes(q) || a.verb.toLowerCase().includes(q),
    );
  }, [verbActions, filter]);

  const toggleMaster = () => {
    if (masterChecked) {
      // Currently fully selected → unselect everything for this resource.
      const verbNames = verbActions.map((a) => a.name);
      onChange(currentActions.filter((a) => a !== wildcard && !verbNames.includes(a)));
    } else {
      // Switch to the compact wildcard form; drop any individual verbs
      // and any prior wildcard so the document stays canonical.
      const verbNames = verbActions.map((a) => a.name);
      const cleaned = currentActions.filter((a) => a !== wildcard && !verbNames.includes(a));
      onChange([...cleaned, wildcard]);
    }
  };

  const toggleVerb = (actionName: string) => {
    if (currentActions.includes(actionName)) {
      onChange(currentActions.filter((a) => a !== actionName));
      return;
    }
    // If the wildcard is active, expand it to its constituents minus the
    // verb the user is just deselecting. (Adding an already-covered verb
    // shouldn't happen, but be defensive.)
    if (hasWildcard) {
      const expanded = verbActions.map((a) => a.name);
      const without = currentActions.filter((a) => a !== wildcard);
      onChange([...without, ...expanded.filter((a) => a !== actionName), actionName]);
      return;
    }
    onChange([...currentActions, actionName]);
  };

  return (
    <div className={styles.panel} role="region" aria-label={t('pages:iam.scopedActionsAria', { resource: resource.type })}>
      <div className={styles.subtitle}>
        {t('pages:iam.scopedActionsSubtitle', { resource: resource.type })}
      </div>

      <Input
        type="search"
        className={styles.filterInput}
        placeholder={t('pages:iam.scopedActionsFilterPlaceholder')}
        value={filter}
        onChange={(e) => setFilter(e.target.value)}
        aria-label={t('pages:iam.scopedActionsFilterAria')}
      />

      <label className={masterChecked ? styles.masterRowActive : styles.masterRow}>
        <input
          type="checkbox"
          checked={masterChecked}
          ref={(el) => {
            if (el) el.indeterminate = masterIndeterminate;
          }}
          onChange={toggleMaster}
        />
        <span className={styles.masterLabel}>
          {t('pages:iam.scopedActionsMaster', { resource: resource.type })}
        </span>
        <code className={styles.masterCode}>{wildcard}</code>
        <span className={styles.masterCount}>
          {selectedCount > 0
            ? t('pages:iam.scopedActionsSelected', {
                selected: selectedCount,
                total: verbActions.length,
              })
            : t('pages:iam.scopedActionsTotal', { total: verbActions.length })}
        </span>
      </label>

      <div className={styles.verbList}>
        {filtered.length === 0 && (
          <div className={styles.emptyState}>{t('pages:iam.scopedActionsNoMatch')}</div>
        )}
        {filtered.map((a) => {
          const checked = hasWildcard || currentActions.includes(a.name);
          return (
            <label key={a.name} className={styles.verbRow}>
              <input
                type="checkbox"
                checked={checked}
                onChange={() => toggleVerb(a.name)}
                disabled={hasWildcard && !currentActions.includes(a.name)}
                title={hasWildcard ? t('pages:iam.scopedActionsCoveredByWildcard') : undefined}
              />
              <span className={styles.verbName}>
                <span className={styles.verbToken}>{a.verb}</span>
                <code className={styles.verbCode}>{a.name}</code>
              </span>
            </label>
          );
        })}
      </div>
    </div>
  );
}
