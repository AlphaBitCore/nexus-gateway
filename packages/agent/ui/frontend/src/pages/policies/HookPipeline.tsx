import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import type { PolicyHook } from '@/api/agent';
import styles from './HookPipeline.module.css';

// HookPipeline renders the full execution chain — grouped by stage,
// sorted by priority ascending inside each stage — so the user can
// see "preInbound runs first, then preOutbound, then postOutbound,
// here's the hook ordering inside each". Mirrors the Pipeline tab in
// CP-UI's hook detail screen so the agent-side display matches the
// admin's mental model.
//
// Collapsed by default to keep the Hooks list page scannable; the
// expand toggle is the entire header so it's accessible without
// hunting for a chevron.

const STAGE_ORDER = ['preInbound', 'preOutbound', 'postInbound', 'postOutbound'];

export function HookPipeline({ hooks }: { hooks: PolicyHook[] }) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);

  const grouped = useMemo(() => groupByStage(hooks), [hooks]);
  const enabledCount = hooks.filter((h) => h.enabled).length;

  return (
    <section className={styles.card}>
      <button
        type="button"
        className={styles.header}
        onClick={() => setOpen(!open)}
        aria-expanded={open}
      >
        <span className={styles.headerTitle}>{t('policies.hooks.pipeline.title')}</span>
        <span className={styles.headerMeta}>
          {t('policies.hooks.pipeline.summary', {
            enabled: enabledCount,
            total: hooks.length,
            stages: grouped.length,
          })}
        </span>
        <span className={styles.chevron} aria-hidden>{open ? '▾' : '▸'}</span>
      </button>

      {open && (
        <div className={styles.body}>
          {grouped.length === 0 ? (
            <p className={styles.empty}>{t('policies.hooks.pipeline.empty')}</p>
          ) : (
            <ol className={styles.stages}>
              {grouped.map(({ stage, items }) => (
                <li key={stage} className={styles.stage}>
                  <header className={styles.stageHeader}>
                    <span className={styles.stageName}>{stage}</span>
                    <span className={styles.stageCount}>{t('policies.hooks.pipeline.stageCount', { count: items.length })}</span>
                  </header>
                  <ol className={styles.hookList}>
                    {items.map((h) => (
                      <li key={h.id} className={styles.hookItem} data-disabled={!h.enabled}>
                        <span className={styles.hookPriority}>{h.priority}</span>
                        <Link to={`/policies/hooks/${encodeURIComponent(h.id)}`} className={styles.hookName}>
                          {h.name}
                        </Link>
                        {!h.enabled && (
                          <span className={styles.disabledTag}>
                            {t('policies.hooks.cols.disabled')}
                          </span>
                        )}
                        {h.implementationId && (
                          <span className={styles.hookImpl}>{h.implementationId}</span>
                        )}
                      </li>
                    ))}
                  </ol>
                </li>
              ))}
            </ol>
          )}
        </div>
      )}
    </section>
  );
}

function groupByStage(hooks: PolicyHook[]): { stage: string; items: PolicyHook[] }[] {
  const map = new Map<string, PolicyHook[]>();
  for (const h of hooks) {
    const k = h.stage || '(no stage)';
    if (!map.has(k)) map.set(k, []);
    map.get(k)!.push(h);
  }
  for (const arr of map.values()) {
    arr.sort((a, b) => (a.priority ?? 0) - (b.priority ?? 0));
  }
  // Stages sorted by the canonical execution order; unknown stages
  // get appended last in alphabetical order so future stages still
  // show up sensibly.
  const known = STAGE_ORDER.filter((s) => map.has(s));
  const unknown = [...map.keys()].filter((s) => !STAGE_ORDER.includes(s)).sort();
  return [...known, ...unknown].map((stage) => ({ stage, items: map.get(stage)! }));
}
