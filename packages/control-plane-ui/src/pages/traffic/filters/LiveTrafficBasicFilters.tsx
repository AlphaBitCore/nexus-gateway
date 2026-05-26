import { useTranslation } from 'react-i18next';
import { Chip } from '@nexus-gateway/ui-shared';
import { projectApi, providerApi, systemApi, virtualKeyApi, devicesApi, iamApi, hubApi } from '@/api/services';
import { SearchableCombobox, Button, Input, Stack, type ComboboxOption, OrgTreeSelect } from '@/components/ui';
import type { VirtualKey } from '../../../api/types';
import {
  type LiveTrafficFiltersState,
  type TrafficSourceFilter,
  toDatetimeLocalValue,
} from './liveTrafficFilters';
import { FieldCompact } from './LiveTrafficFilterPanel';
import css from './LiveTrafficFilterPanel.module.css';

const HOOK_OPTIONS = ['APPROVE', 'REJECT_HARD', 'BLOCK_SOFT', 'MODIFY', 'ABSTAIN'] as const;
const BUMP_STATUS_OPTIONS = ['BUMP_SUCCESS', 'BUMP_FAILED_PASSTHROUGH', 'BUMP_EXEMPT_CONFIGURED'] as const;

interface LiveTrafficBasicFiltersProps {
  value: LiveTrafficFiltersState;
  onPatch: (patch: Partial<LiveTrafficFiltersState>) => void;
  source: TrafficSourceFilter;
}

export function LiveTrafficBasicFilters({
  value: v,
  onPatch,
  source,
}: LiveTrafficBasicFiltersProps) {
  const { t } = useTranslation();
  const hasTimeRange = Boolean(v.startTime?.trim() || v.endTime?.trim());
  const modelDisabled = !v._providerId?.trim();

  const activePreset = (() => {
    if (!v.startTime || !v.endTime) return null;
    const start = new Date(v.startTime).getTime();
    const end = new Date(v.endTime).getTime();
    const diffH = (end - start) / 3600_000;
    if (Math.abs(diffH - 1) < 0.1) return 1;
    if (Math.abs(diffH - 24) < 0.5) return 24;
    if (Math.abs(diffH - 168) < 2) return 168;
    return null;
  })();

  const setPresetRange = (hours: number) => {
    const end = new Date();
    const start = new Date(end.getTime() - hours * 3600_000);
    onPatch({ startTime: toDatetimeLocalValue(start), endTime: toDatetimeLocalValue(end) });
  };

  const setPresetDays = (days: number) => {
    const end = new Date();
    const start = new Date(end.getTime() - days * 86400_000);
    onPatch({ startTime: toDatetimeLocalValue(start), endTime: toDatetimeLocalValue(end) });
  };

  return (
    <>
      {/* Time row — always shown */}
      <div className={css.timeRow}>
        <FieldCompact label={t('pages:traffic.labelQuickRange')} tip={t('pages:traffic.tipQuickRange')}>
          <Stack direction="horizontal" gap="xs">
            <Chip size="sm" active={activePreset === 1} onClick={() => setPresetRange(1)}>1h</Chip>
            <Chip size="sm" active={activePreset === 24} onClick={() => setPresetRange(24)}>24h</Chip>
            <Chip size="sm" active={activePreset === 168} onClick={() => setPresetDays(7)}>7d</Chip>
            {hasTimeRange ? (
              <Button
                variant="ghost"
                size="sm"
                aria-label={t('pages:traffic.clearTime')}
                onClick={() => onPatch({ startTime: '', endTime: '' })}
              >
                {t('pages:traffic.clearTime')}
              </Button>
            ) : null}
          </Stack>
        </FieldCompact>
        <FieldCompact label={t('pages:traffic.labelFrom')} tip={t('pages:traffic.tipFrom')}>
          <Input
            type="datetime-local"
            aria-label={t('pages:traffic.labelFrom')}
            value={v.startTime}
            onChange={(e) => onPatch({ startTime: e.target.value })}
            className={css.dtInput}
          />
        </FieldCompact>
        <FieldCompact label={t('pages:traffic.labelTo')} tip={t('pages:traffic.tipTo')}>
          <Input
            type="datetime-local"
            aria-label={t('pages:traffic.labelTo')}
            value={v.endTime}
            onChange={(e) => onPatch({ endTime: e.target.value })}
            className={css.dtInput}
          />
        </FieldCompact>
      </div>

      {/* Node / Thing — present for every source because traffic_event.thing_id
          is populated by all three data-plane producers. The dropdown filters
          its list by source-appropriate node type so users only see relevant
          choices for the active tab. Driven by the same `thingId` filter
          state that node-detail cross-links populate via the URL. */}
      <div className={css.primaryGrid}>
        <FieldCompact label={t('pages:traffic.labelNode')} tip={t('pages:traffic.tipNode')}>
          <SearchableCombobox
            ariaLabel={t('pages:traffic.labelNode')}
            placeholder={t('pages:traffic.placeholderNode')}
            valueId={v.thingId}
            valueLabel={v._thingLabel}
            allowEmptyQueryFetch
            fetchOptions={async (q) => {
              const nodeType =
                source === 'vk' ? 'ai-gateway'
                  : source === 'proxy' ? 'compliance-proxy'
                  : source === 'agent' ? 'agent'
                  : undefined;
              const params: { type?: string; search?: string; pageSize?: number } = { pageSize: 200 };
              if (nodeType) params.type = nodeType;
              if (q.trim()) params.search = q.trim();
              const res = await hubApi.listNodes(params);
              const rows = res.nodes ?? [];
              return rows.slice(0, 80).map((n) => ({
                id: n.id,
                label: `${n.name} (${n.type})`,
              }));
            }}
            onSelect={(opt) =>
              onPatch({ thingId: opt?.id ?? '', _thingLabel: opt?.label ?? '' })
            }
          />
        </FieldCompact>
      </div>

      {source === 'vk' && (
        <>
          {/* Row 1: Organization → Project → Virtual Key */}
          <div className={css.primaryGrid}>
            <FieldCompact label={t('pages:traffic.labelOrganization')} tip={t('pages:traffic.tipOrganization')}>
              <OrgTreeSelect
                value={v.orgId}
                onChange={(val) => onPatch({ orgId: val as string, _orgLabel: '' })}
                allowClear
              />
            </FieldCompact>
            <FieldCompact label={t('pages:traffic.stepProject')} tip={t('pages:traffic.tipProject')}>
              <SearchableCombobox
                ariaLabel={t('pages:traffic.stepProject')}
                placeholder={t('pages:traffic.placeholderProjectEnabled')}
                valueId={v.projectId}
                valueLabel={v._projectLabel}
                allowEmptyQueryFetch
                fetchOptions={async (q) => {
                  const res = await projectApi.list({
                    ...(q.trim() && { q: q.trim() }),
                  });
                  const rows = res.data ?? [];
                  return rows.slice(0, 120).map((p) => ({
                    id: p.id,
                    label: `${p.name} (${p.code})`,
                  }));
                }}
                onSelect={(opt) => {
                  if (!opt) {
                    onPatch({ projectId: '', _projectLabel: '', virtualKeyId: '', _vkLabel: '' });
                    return;
                  }
                  onPatch({ projectId: opt.id, _projectLabel: opt.label, virtualKeyId: '', _vkLabel: '' });
                }}
              />
            </FieldCompact>
            <FieldCompact
              label={t('pages:traffic.labelVirtualKey')}
              tip={v.projectId ? t('pages:traffic.tipVirtualKeyScoped') : t('pages:traffic.tipVirtualKeyDefault')}
            >
              <SearchableCombobox
                ariaLabel={t('pages:traffic.labelVirtualKey')}
                placeholder={t('pages:traffic.placeholderVirtualKey')}
                valueId={v.virtualKeyId}
                valueLabel={v._vkLabel}
                allowEmptyQueryFetch
                fetchOptions={async (q) => {
                  const params: Record<string, string> = { limit: '200' };
                  if (q.trim()) params.q = q.trim();
                  if (v.projectId.trim()) params.projectId = v.projectId.trim();
                  const res = await virtualKeyApi.list(params) as {
                    data: Array<VirtualKey & { project?: { name?: string } }>;
                    total: number;
                  };
                  const rows = res.data ?? [];
                  return rows.slice(0, 80).map((k) => ({
                    id: k.id,
                    label: `${k.name}${k.project?.name ? ` · ${k.project.name}` : ''}`,
                  }));
                }}
                onSelect={(opt) => {
                  if (!opt) { onPatch({ virtualKeyId: '', _vkLabel: '' }); return; }
                  onPatch({ virtualKeyId: opt.id, _vkLabel: opt.label });
                }}
              />
            </FieldCompact>
          </div>

          {/* Row 2: Provider → Model → Hook Decision */}
          <div className={css.primaryGrid}>
            <FieldCompact label={t('pages:traffic.labelProvider')} tip={t('pages:traffic.tipProvider')}>
              <SearchableCombobox
                ariaLabel={t('pages:traffic.labelProvider')}
                placeholder={t('pages:traffic.placeholderProvider')}
                valueId={v._providerId}
                valueLabel={v.provider}
                allowEmptyQueryFetch
                fetchOptions={async (q) => {
                  const res = await providerApi.list({ limit: '200', ...(q.trim() && { q: q.trim() }) });
                  const rows = res.data ?? [];
                  const term = q.trim().toLowerCase();
                  const filtered = term ? rows.filter((p) => p.name.toLowerCase().includes(term)) : rows;
                  return filtered.slice(0, 80).map((p) => ({ id: p.id, label: p.name }));
                }}
                onSelect={(opt) =>
                  onPatch({ provider: opt?.label ?? '', _providerId: opt?.id ?? '', modelUsed: '', _modelLabel: '' })
                }
              />
            </FieldCompact>
            <FieldCompact
              label={t('pages:traffic.labelModel')}
              tip={modelDisabled ? t('pages:traffic.tipModelDisabled') : t('pages:traffic.tipModelEnabled')}
            >
              <SearchableCombobox
                ariaLabel={t('pages:traffic.labelModel')}
                placeholder={modelDisabled ? t('pages:traffic.placeholderModelDisabled') : t('pages:traffic.placeholderModelEnabled')}
                disabled={modelDisabled}
                valueId={v.modelUsed}
                valueLabel={v._modelLabel || v.modelUsed}
                allowEmptyQueryFetch={!modelDisabled}
                fetchOptions={async (q) => {
                  const pid = v._providerId.trim();
                  if (!pid) return [];
                  const res = await systemApi.listModels({
                    providerId: pid,
                    ...(q.trim() && { q: q.trim() }),
                  }) as {
                    data: Array<{
                      provider: { name: string };
                      models: Array<{ id: string; name: string; providerModelId: string }>;
                    }>;
                  };
                  const out: ComboboxOption[] = [];
                  for (const g of res.data ?? []) {
                    const pn = g.provider?.name ?? 'provider';
                    for (const m of g.models ?? []) {
                      const apiId = (m.providerModelId ?? '').trim() || m.id;
                      out.push({ id: apiId, label: `${pn} / ${m.name}` });
                    }
                  }
                  return out.slice(0, 100);
                }}
                onSelect={(opt) => onPatch({ modelUsed: opt?.id ?? '', _modelLabel: opt?.label ?? '' })}
              />
            </FieldCompact>
            <FieldCompact label={t('pages:traffic.labelHookDecision')} tip={t('pages:traffic.tipHookDecision')}>
              <select
                aria-label={t('pages:traffic.labelHookDecision')}
                value={v.requestHookDecision}
                onChange={(e) => onPatch({ requestHookDecision: e.target.value })}
                className={css.nativeSelect}
              >
                <option value="">{t('pages:traffic.optionAll')}</option>
                {HOOK_OPTIONS.map((h) => (
                  <option key={h} value={h}>{h}</option>
                ))}
              </select>
            </FieldCompact>
          </div>

          {/* Row 3: Target Host + Path */}
          <div className={css.primaryGrid}>
            <FieldCompact label={t('pages:traffic.labelTargetHost')} tip={t('pages:traffic.tipTargetHost')}>
              <Input
                type="text"
                aria-label={t('pages:traffic.labelTargetHost')}
                value={v.targetHost}
                onChange={(e) => onPatch({ targetHost: e.target.value })}
                placeholder={t('pages:traffic.placeholderTargetHost')}
                className={css.dtInput}
              />
            </FieldCompact>
            <FieldCompact label={t('pages:traffic.labelPath')} tip={t('pages:traffic.tipPath')}>
              <Input
                type="text"
                aria-label={t('pages:traffic.labelPath')}
                value={v.path}
                onChange={(e) => onPatch({ path: e.target.value })}
                placeholder={t('pages:traffic.placeholderPath')}
                className={css.dtInput}
              />
            </FieldCompact>
          </div>
        </>
      )}

      {source === 'proxy' && (
        /* Single row: Target Host | Path | Bump Status | Hook Decision */
        <div className={css.primaryGrid4}>
          <FieldCompact label={t('pages:traffic.labelTargetHost')} tip={t('pages:traffic.tipTargetHost')}>
            <Input
              type="text"
              aria-label={t('pages:traffic.labelTargetHost')}
              value={v.targetHost}
              onChange={(e) => onPatch({ targetHost: e.target.value })}
              placeholder={t('pages:traffic.placeholderTargetHost')}
              className={css.dtInput}
            />
          </FieldCompact>
          <FieldCompact label={t('pages:traffic.labelPath')} tip={t('pages:traffic.tipPath')}>
            <Input
              type="text"
              aria-label={t('pages:traffic.labelPath')}
              value={v.path}
              onChange={(e) => onPatch({ path: e.target.value })}
              placeholder={t('pages:traffic.placeholderPath')}
              className={css.dtInput}
            />
          </FieldCompact>
          <FieldCompact label={t('pages:traffic.labelBumpStatus')} tip={t('pages:traffic.tipBumpStatus')}>
            <select
              aria-label={t('pages:traffic.labelBumpStatus')}
              value={v.bumpStatus}
              onChange={(e) => onPatch({ bumpStatus: e.target.value })}
              className={css.nativeSelect}
            >
              <option value="">{t('pages:traffic.optionAny')}</option>
              {BUMP_STATUS_OPTIONS.map((s) => (
                <option key={s} value={s}>{s}</option>
              ))}
            </select>
          </FieldCompact>
          <FieldCompact label={t('pages:traffic.labelHookDecision')} tip={t('pages:traffic.tipHookDecision')}>
            <select
              aria-label={t('pages:traffic.labelHookDecision')}
              value={v.requestHookDecision}
              onChange={(e) => onPatch({ requestHookDecision: e.target.value })}
              className={css.nativeSelect}
            >
              <option value="">{t('pages:traffic.optionAll')}</option>
              {HOOK_OPTIONS.map((h) => (
                <option key={h} value={h}>{h}</option>
              ))}
            </select>
          </FieldCompact>
        </div>
      )}

      {source === 'agent' && (
        <>
          {/* Row 1: Target Host | Path | Device */}
          <div className={css.primaryGrid}>
            <FieldCompact label={t('pages:traffic.labelTargetHost')} tip={t('pages:traffic.tipTargetHost')}>
              <Input
                type="text"
                aria-label={t('pages:traffic.labelTargetHost')}
                value={v.targetHost}
                onChange={(e) => onPatch({ targetHost: e.target.value })}
                placeholder={t('pages:traffic.placeholderTargetHost')}
                className={css.dtInput}
              />
            </FieldCompact>
            <FieldCompact label={t('pages:traffic.labelPath')} tip={t('pages:traffic.tipPath')}>
              <Input
                type="text"
                aria-label={t('pages:traffic.labelPath')}
                value={v.path}
                onChange={(e) => onPatch({ path: e.target.value })}
                placeholder={t('pages:traffic.placeholderPath')}
                className={css.dtInput}
              />
            </FieldCompact>
            <FieldCompact label={t('pages:traffic.labelDevice')} tip={t('pages:traffic.tipDevice')}>
              <SearchableCombobox
                ariaLabel={t('pages:traffic.labelDevice')}
                placeholder={t('pages:traffic.placeholderSelectDevice')}
                valueId={v.deviceId}
                valueLabel={v._deviceLabel}
                allowEmptyQueryFetch
                fetchOptions={async (q) => {
                  const res = await devicesApi.list({ limit: '200' });
                  const rows = res.data ?? [];
                  const term = q.trim().toLowerCase();
                  const filtered = term ? rows.filter((d) => d.hostname.toLowerCase().includes(term)) : rows;
                  return filtered.slice(0, 80).map((d) => ({ id: d.id, label: d.hostname }));
                }}
                onSelect={(opt) => onPatch({ deviceId: opt?.id ?? '', _deviceLabel: opt?.label ?? '' })}
              />
            </FieldCompact>
          </div>

          {/* Row 2: Source Process | Hook Decision */}
          <div className={css.primaryGrid}>
            <FieldCompact label={t('pages:traffic.labelSourceProcess')} tip={t('pages:traffic.tipSourceProcess')}>
              <Input
                type="text"
                aria-label={t('pages:traffic.labelSourceProcess')}
                value={v.sourceProcess}
                onChange={(e) => onPatch({ sourceProcess: e.target.value })}
                placeholder={t('pages:traffic.placeholderProcessExample')}
                className={css.dtInput}
              />
            </FieldCompact>
            <FieldCompact label={t('pages:traffic.labelHookDecision')} tip={t('pages:traffic.tipHookDecision')}>
              <select
                aria-label={t('pages:traffic.labelHookDecision')}
                value={v.requestHookDecision}
                onChange={(e) => onPatch({ requestHookDecision: e.target.value })}
                className={css.nativeSelect}
              >
                <option value="">{t('pages:traffic.optionAll')}</option>
                {HOOK_OPTIONS.map((h) => (
                  <option key={h} value={h}>{h}</option>
                ))}
              </select>
            </FieldCompact>
          </div>
        </>
      )}

      {source === '' && (
        <>
          {/* Row 1: Target Host | Path */}
          <div className={css.primaryGrid}>
            <FieldCompact label={t('pages:traffic.labelTargetHost')} tip={t('pages:traffic.tipTargetHost')}>
              <Input
                type="text"
                aria-label={t('pages:traffic.labelTargetHost')}
                value={v.targetHost}
                onChange={(e) => onPatch({ targetHost: e.target.value })}
                placeholder={t('pages:traffic.placeholderTargetHost')}
                className={css.dtInput}
              />
            </FieldCompact>
            <FieldCompact label={t('pages:traffic.labelPath')} tip={t('pages:traffic.tipPath')}>
              <Input
                type="text"
                aria-label={t('pages:traffic.labelPath')}
                value={v.path}
                onChange={(e) => onPatch({ path: e.target.value })}
                placeholder={t('pages:traffic.placeholderPath')}
                className={css.dtInput}
              />
            </FieldCompact>
          </div>

          {/* Row 2: User | Organization | Hook Decision */}
          <div className={css.primaryGrid}>
            <FieldCompact label={t('pages:traffic.labelUser')} tip={t('pages:traffic.tipUser')}>
              <SearchableCombobox
                ariaLabel={t('pages:traffic.labelUser')}
                placeholder={t('pages:traffic.placeholderSearchUser')}
                valueId={v.userId}
                valueLabel={v._userLabel}
                allowEmptyQueryFetch
                fetchOptions={async (q) => {
                  const params: Record<string, string> = { limit: '100' };
                  if (q.trim()) params.q = q.trim();
                  const res = await iamApi.listUsers(params);
                  const rows = res.data ?? [];
                  return rows.map((u) => ({
                    id: u.id,
                    label: u.displayName + (u.email ? ` (${u.email})` : ''),
                  }));
                }}
                onSelect={(opt) => onPatch({ userId: opt?.id ?? '', _userLabel: opt?.label ?? '' })}
              />
            </FieldCompact>
            <FieldCompact label={t('pages:traffic.labelOrganization')} tip={t('pages:traffic.tipOrganization')}>
              <OrgTreeSelect
                value={v.orgId}
                onChange={(val) => onPatch({ orgId: val as string, _orgLabel: '' })}
                allowClear
              />
            </FieldCompact>
            <FieldCompact label={t('pages:traffic.labelHookDecision')} tip={t('pages:traffic.tipHookDecision')}>
              <select
                aria-label={t('pages:traffic.labelHookDecision')}
                value={v.requestHookDecision}
                onChange={(e) => onPatch({ requestHookDecision: e.target.value })}
                className={css.nativeSelect}
              >
                <option value="">{t('pages:traffic.optionAll')}</option>
                {HOOK_OPTIONS.map((h) => (
                  <option key={h} value={h}>{h}</option>
                ))}
              </select>
            </FieldCompact>
          </div>
        </>
      )}
    </>
  );
}
