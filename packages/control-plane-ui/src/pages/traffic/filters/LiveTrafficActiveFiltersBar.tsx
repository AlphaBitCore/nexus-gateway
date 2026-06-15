import { useTranslation } from 'react-i18next';
import { countLiveTrafficFilters, describeLiveTrafficFilters, statusRangeLabelKey, type LiveTrafficFiltersState } from './liveTrafficFilters';
import css from './LiveTrafficActiveFiltersBar.module.css';

interface ActiveFilterChip {
  id: string;
  label: string;
  patch: Partial<LiveTrafficFiltersState>;
}

type TFn = (key: string, opts?: Record<string, unknown>) => string;

// fallbackLines come from describeLiveTrafficFilters (internal, English) and are
// parsed only to recover the formatted time value — the visible label text is
// translated. tr resolves pages:traffic.activeFilterLabels.* keys.
function buildRemovableChips(applied: LiveTrafficFiltersState, fallbackLines: string[], tr: TFn): ActiveFilterChip[] {
  const chips: ActiveFilterChip[] = [];
  const v = (s: string) => s.trim();
  const lbl = (key: string) => tr(`pages:traffic.activeFilterLabels.${key}`);
  const add = (id: string, label: string, patch: Partial<LiveTrafficFiltersState>) => {
    if (label) chips.push({ id, label, patch });
  };

  add('provider', v(applied.provider) ? `${lbl('provider')}: ${v(applied.provider)}` : '', { provider: '', _providerId: '', modelUsed: '', _modelLabel: '' });
  add('virtualKeyId', v(applied.virtualKeyId) ? `${lbl('virtualKey')}: ${v(applied._vkLabel) || `${v(applied.virtualKeyId).slice(0, 8)}...`}` : '', { virtualKeyId: '', _vkLabel: '' });
  add('userId', !v(applied.virtualKeyId) && v(applied.userId) ? `${lbl('user')}: ${v(applied._userLabel) || `${v(applied.userId).slice(0, 8)}...`}` : '', { userId: '', _userLabel: '' });
  add('orgId', v(applied.orgId) ? `${lbl('organization')}: ${v(applied._orgLabel) || `${v(applied.orgId).slice(0, 8)}...`}` : '', { orgId: '', _orgLabel: '' });
  add('projectId', v(applied.projectId) ? `${lbl('project')}: ${v(applied._projectLabel) || `${v(applied.projectId).slice(0, 8)}...`}` : '', { projectId: '', _projectLabel: '', virtualKeyId: '', _vkLabel: '' });
  add('modelUsed', v(applied.modelUsed) ? `${lbl('model')}: ${v(applied._modelLabel) || v(applied.modelUsed)}` : '', { modelUsed: '', _modelLabel: '' });
  add('requestId', v(applied.requestId) ? `${lbl('requestId')}: ${v(applied.requestId)}` : '', { requestId: '' });
  add('requestHookDecision', v(applied.requestHookDecision) ? `${lbl('requestHook')}: ${v(applied.requestHookDecision)}` : '', { requestHookDecision: '' });
  add('responseHookDecision', v(applied.responseHookDecision) ? `${lbl('responseHook')}: ${v(applied.responseHookDecision)}` : '', { responseHookDecision: '' });
  add('statusCode', v(applied.statusCode) ? `${lbl('http')} ${v(applied.statusCode)}` : '', { statusCode: '' });
  add('statusRange', !v(applied.statusCode) && applied.statusRange ? `${lbl('http')}: ${lbl(statusRangeLabelKey(applied.statusRange))}` : '', { statusRange: '' });
  add('cacheStatus', applied.cacheStatus ? `${lbl('cache')}: ${applied.cacheStatus}` : '', { cacheStatus: '' });
  if (v(applied.startTime) || v(applied.endTime)) {
    const any = lbl('any');
    const from = fallbackLines.find((line) => line.startsWith('From:'))?.replace(/^From:\s*/, '') || v(applied.startTime);
    const to = fallbackLines.find((line) => line.startsWith('To:'))?.replace(/^To:\s*/, '') || v(applied.endTime);
    add('timeRange', `${lbl('time')}: ${from || any} - ${to || any}`, { startTime: '', endTime: '' });
  }
  add('targetHost', v(applied.targetHost) ? `${lbl('target')}: ${v(applied.targetHost)}` : '', { targetHost: '' });
  add('path', v(applied.path) ? `${lbl('path')}: ${v(applied.path)}` : '', { path: '' });
  add('deviceId', v(applied.deviceId) ? `${lbl('device')}: ${v(applied._deviceLabel) || `${v(applied.deviceId).slice(0, 8)}...`}` : '', { deviceId: '', _deviceLabel: '' });
  add('thingId', v(applied.thingId) ? `${lbl('node')}: ${v(applied._thingLabel) || v(applied.thingId)}` : '', { thingId: '', _thingLabel: '' });
  add('sourceProcess', v(applied.sourceProcess) ? `${lbl('process')}: ${v(applied.sourceProcess)}` : '', { sourceProcess: '' });
  add('bumpStatus', v(applied.bumpStatus) ? `${lbl('bumpStatus')}: ${v(applied.bumpStatus)}` : '', { bumpStatus: '' });
  for (const tag of applied.complianceTags) {
    const trimmed = tag.trim();
    if (trimmed) {
      add(`tag:${trimmed}`, `${lbl('complianceTag')}: ${trimmed}`, {
        complianceTags: applied.complianceTags.filter((item) => item !== tag),
      });
    }
  }

  return chips;
}

export function LiveTrafficActiveFiltersBar({
  applied,
  onRemove,
  onClearAll,
}: {
  applied: LiveTrafficFiltersState;
  onRemove: (patch: Partial<LiveTrafficFiltersState>) => void;
  onClearAll: () => void;
}) {
  const { t } = useTranslation();
  const n = countLiveTrafficFilters(applied);
  if (n === 0) return null;
  const lines = describeLiveTrafficFilters(applied);
  const visibleChips = buildRemovableChips(applied, lines, t);

  return (
    <div className={css.wrapper}>
      <button type="button" className={css.clearAll} onClick={onClearAll}>
        {t('pages:traffic.clearFilters', 'Clear All')}
      </button>
      <div className={css.chipList}>
        {visibleChips.map((chip) => (
          <span key={chip.id} className={css.chip} title={chip.label}>
            <span>{chip.label}</span>
            <button
              type="button"
              className={css.removeChip}
              aria-label={`${t('pages:traffic.clearFilters')} ${chip.label}`}
              onClick={() => onRemove(chip.patch)}
            >
              ×
            </button>
          </span>
        ))}
      </div>
    </div>
  );
}
