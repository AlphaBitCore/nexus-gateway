import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button, Checkbox, Dialog } from '@/components/ui';
import { providerApi, systemApi } from '@/api/services';
import { useMutation } from '@/hooks/useMutation';
import type { Model, Provider } from '@/api/types';
import {
  buildCatalogSyncDiff,
  catalogCreateInput,
  catalogUpdateInput,
  resolveTemplateName,
  type CatalogField,
  type CatalogFieldDiff,
  type CatalogSyncDiff,
  type CatalogValue,
} from './catalog-sync';
import styles from './CatalogSyncDialog.module.css';

interface CatalogSyncDialogProps {
  open: boolean;
  onClose: () => void;
  provider: Provider;
  models: Model[];
  /**
   * Called once an apply settles — including a partial failure, where some rows
   * committed before another was rejected — so the tab refetches its rows and
   * shows what actually exists.
   */
  onApplied: () => void;
}

type LoadState =
  | { kind: 'loading' }
  | { kind: 'no-template' }
  | { kind: 'error' }
  | { kind: 'ready'; diff: CatalogSyncDiff };

/**
 * Field labels reuse the strings the models tab already shows for the same
 * values. Typed against every template field, so a field added to the catalog
 * cannot reach the diff without a label — the diff would otherwise render a
 * raw property name at an admin.
 *
 * The identity fields carry a label too: they are excluded from the diff, not
 * from the type, and a label costs nothing next to a lookup that throws.
 */
const FIELD_LABEL_KEYS: Record<CatalogField, string> = {
  name: 'pages:providers.tableHeaderName',
  description: 'pages:providers.modelDescriptionLabel',
  type: 'pages:providers.type',
  features: 'pages:providers.features',
  inputPricePerMillion: 'pages:providers.modelTableInputPrice',
  outputPricePerMillion: 'pages:providers.modelTableOutputPrice',
  cachedInputReadPricePerMillion: 'pages:providers.modelTableCachedInputReadPrice',
  cachedInputWritePricePerMillion: 'pages:providers.modelTableCachedInputWritePrice',
  audioInputPricePerMillion: 'pages:providers.audioInputPricePerM',
  audioOutputPricePerMillion: 'pages:providers.audioOutputPricePerM',
  cachedAudioInputReadPricePerMillion: 'pages:providers.cachedAudioReadPricePerM',
  maxContextTokens: 'pages:providers.modelTableContext',
  maxOutputTokens: 'pages:providers.modelTableOutput',
  inputModalities: 'pages:providers.capabilities.modalitiesInput',
  outputModalities: 'pages:providers.capabilities.modalitiesOutput',
  requiredModalities: 'pages:providers.capabilities.modalitiesRequired',
  code: 'pages:providers.modelCodeLabel',
  providerModelId: 'pages:providers.providerModelIdLabel',
  aliases: 'pages:providers.modelAliasesLabel',
};

const formatValue = (v: CatalogValue | undefined, emptyLabel: string): string => {
  if (v === undefined || v === null || v === '') return emptyLabel;
  if (Array.isArray(v)) return v.length ? v.join(', ') : emptyLabel;
  return String(v);
};

export function CatalogSyncDialog({ open, onClose, provider, models, onApplied }: CatalogSyncDialogProps) {
  const { t } = useTranslation();
  const [state, setState] = useState<LoadState>({ kind: 'loading' });
  /** Keys of the rows the admin has accepted. New rows start accepted; changed rows do not. */
  const [selected, setSelected] = useState<Set<string>>(new Set());

  // The diff is a snapshot taken when the dialog opens. Reading the rows via a
  // ref keeps them out of the effect's dependencies — the tab hands down a
  // fresh array identity on every render, which would otherwise re-run the
  // fetch in a loop.
  const modelsRef = useRef(models);
  modelsRef.current = models;

  const { id: providerId, name, adapterType, baseUrl } = provider;

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setState({ kind: 'loading' });
    setSelected(new Set());

    (async () => {
      try {
        const { data: templates } = await providerApi.getTemplates();
        const templateName = resolveTemplateName({ name, adapterType, baseUrl }, templates);
        if (!templateName) {
          if (!cancelled) setState({ kind: 'no-template' });
          return;
        }
        const detail = await providerApi.getTemplateDetail(templateName);
        if (cancelled) return;
        const diff = buildCatalogSyncDiff(modelsRef.current, detail.models ?? []);
        setState({ kind: 'ready', diff });
        // A model the provider lacks is a safe default-yes. A changed model is
        // not: the admin may have overridden the catalog deliberately, so the
        // move has to be accepted per row.
        setSelected(new Set(diff.newRows.map((r) => r.key)));
      } catch {
        if (!cancelled) setState({ kind: 'error' });
      }
    })();

    return () => { cancelled = true; };
  }, [open, name, adapterType, baseUrl]);

  const toggle = useCallback((key: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  }, []);

  // Rows are written one at a time against per-row endpoints, so an apply can
  // stop half-committed — a model code that already exists on another provider
  // is rejected while its neighbours succeed. Each row is therefore isolated:
  // one rejection must not abandon the rows behind it, and whatever did commit
  // has to be reported and refetched rather than silently dropped.
  const { mutate: applySync, loading: applying } = useMutation(
    async ({ diff, accepted }: { diff: CatalogSyncDiff; accepted: Set<string> }) => {
      const failed: string[] = [];
      let applied = 0;

      for (const row of diff.newRows) {
        if (!accepted.has(row.key)) continue;
        try {
          await providerApi.addModel(providerId, catalogCreateInput(row.template));
          applied += 1;
        } catch {
          failed.push(row.template.name);
        }
      }
      for (const row of diff.changedRows) {
        if (!accepted.has(row.key)) continue;
        try {
          await systemApi.updateModel(row.model.id, catalogUpdateInput(row.diffs));
          applied += 1;
        } catch {
          failed.push(row.model.name);
        }
      }

      // Reported as a failure so the admin is never told a partial apply
      // succeeded; the count carries what did commit.
      if (failed.length > 0) {
        throw new Error(
          t('pages:providers.catalogSyncPartialFailure', {
            applied,
            total: applied + failed.length,
            failed: failed.join(', '),
          }),
        );
      }
    },
    {
      // Both outcomes refetch and close. The diff is a snapshot taken when the
      // dialog opened, so leaving it on screen after rows have committed would
      // show an apply that is no longer offered — and re-applying it would be
      // rejected as a duplicate. Reopening recomputes the diff from the rows
      // that now exist, which is the recovery path for the failed ones.
      onSuccess: () => {
        onApplied();
        onClose();
      },
      onError: () => {
        onApplied();
        onClose();
      },
      successMessage: t('pages:providers.catalogSyncApplied'),
    },
  );

  const emptyLabel = t('pages:providers.catalogSyncNoValue');

  const renderDiff = (d: CatalogFieldDiff) => (
    <div key={d.field} className={styles.diffItem}>
      <span className={styles.diffField}>{t(FIELD_LABEL_KEYS[d.field])}</span>
      <span className={styles.diffFrom}>{formatValue(d.current, emptyLabel)}</span>
      <span className={styles.diffArrow} aria-hidden="true">{'→'}</span>
      <span className={styles.diffTo}>{formatValue(d.catalog, emptyLabel)}</span>
    </div>
  );

  const body = () => {
    if (state.kind === 'loading') return <div className={styles.state}>{t('common:loading')}</div>;
    if (state.kind === 'error') return <div className={styles.state}>{t('pages:providers.catalogSyncLoadFailed')}</div>;
    if (state.kind === 'no-template') return <div className={styles.state}>{t('pages:providers.catalogSyncNoTemplate')}</div>;

    const { newRows, changedRows, providerOnlyRows } = state.diff;
    if (newRows.length === 0 && changedRows.length === 0 && providerOnlyRows.length === 0) {
      return <div className={styles.state}>{t('pages:providers.catalogSyncInSync')}</div>;
    }

    return (
      <div className={styles.sections}>
        {newRows.length > 0 && (
          <section className={styles.section}>
            <div className={styles.sectionHead}>
              <div className={styles.sectionTitle}>
                {t('pages:providers.catalogSyncSectionNew', { count: newRows.length })}
              </div>
              <div className={styles.sectionHint}>{t('pages:providers.catalogSyncSectionNewHint')}</div>
            </div>
            {newRows.map((row) => (
              <label key={row.key} className={styles.row}>
                <Checkbox checked={selected.has(row.key)} onCheckedChange={() => toggle(row.key)} />
                <div className={styles.rowBody}>
                  <div className={styles.rowName}>{row.template.name}</div>
                  <div className={styles.rowId}>{row.template.providerModelId}</div>
                </div>
              </label>
            ))}
          </section>
        )}

        {changedRows.length > 0 && (
          <section className={styles.section}>
            <div className={styles.sectionHead}>
              <div className={styles.sectionTitle}>
                {t('pages:providers.catalogSyncSectionChanged', { count: changedRows.length })}
              </div>
              <div className={styles.sectionHint}>{t('pages:providers.catalogSyncSectionChangedHint')}</div>
            </div>
            {changedRows.map((row) => (
              <label key={row.key} className={styles.row}>
                <Checkbox checked={selected.has(row.key)} onCheckedChange={() => toggle(row.key)} />
                <div className={styles.rowBody}>
                  <div className={styles.rowName}>{row.model.name}</div>
                  <div className={styles.rowId}>{row.model.providerModelId}</div>
                  <div className={styles.diffList}>{row.diffs.map(renderDiff)}</div>
                </div>
              </label>
            ))}
          </section>
        )}

        {providerOnlyRows.length > 0 && (
          <section className={styles.section}>
            <div className={styles.sectionHead}>
              <div className={styles.sectionTitle}>
                {t('pages:providers.catalogSyncSectionProviderOnly', { count: providerOnlyRows.length })}
              </div>
              <div className={styles.sectionHint}>{t('pages:providers.catalogSyncSectionProviderOnlyHint')}</div>
            </div>
            {providerOnlyRows.map((row) => (
              <div key={row.key} className={styles.row}>
                <div className={styles.rowBody}>
                  <div className={styles.rowName}>{row.model.name}</div>
                  <div className={styles.rowId}>{row.model.providerModelId}</div>
                </div>
              </div>
            ))}
          </section>
        )}
      </div>
    );
  };

  const acceptedCount = state.kind === 'ready'
    ? [...state.diff.newRows, ...state.diff.changedRows].filter((r) => selected.has(r.key)).length
    : 0;

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => { if (!next) onClose(); }}
      title={t('pages:providers.catalogSyncTitle')}
      description={t('pages:providers.catalogSyncDescription')}
      size="lg"
      footer={
        <div className={styles.footer}>
          <Button variant="secondary" onClick={onClose} disabled={applying}>
            {t('common:cancel')}
          </Button>
          <Button
            onClick={() => {
              if (state.kind === 'ready') {
                // Rejection is surfaced by the mutation's error toast; the
                // handler only has to keep it from escaping unhandled.
                applySync({ diff: state.diff, accepted: selected }).catch(() => undefined);
              }
            }}
            disabled={state.kind !== 'ready' || acceptedCount === 0 || applying}
          >
            {t('pages:providers.catalogSyncApply', { count: acceptedCount })}
          </Button>
        </div>
      }
    >
      {body()}
    </Dialog>
  );
}
