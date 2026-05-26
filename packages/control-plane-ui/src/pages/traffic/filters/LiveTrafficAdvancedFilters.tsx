import { useTranslation } from 'react-i18next';
import { Input } from '@/components/ui';
import type { LiveTrafficFiltersState } from '../filters/liveTrafficFilters';
import type { TrafficSourceFilter } from '../filters/liveTrafficFilters';
import { FieldCompact } from '../filters/LiveTrafficFilterPanel';
import { ComplianceTagChipInput } from '../list/ComplianceTagChips';
import css from './LiveTrafficFilterPanel.module.css';

const HOOK_OPTIONS = ['APPROVE', 'REJECT_HARD', 'BLOCK_SOFT', 'MODIFY', 'ABSTAIN'] as const;
const BUMP_STATUS_OPTIONS = ['BUMP_SUCCESS', 'BUMP_FAILED_PASSTHROUGH', 'BUMP_EXEMPT_CONFIGURED'] as const;

interface LiveTrafficAdvancedFiltersProps {
  value: LiveTrafficFiltersState;
  onPatch: (patch: Partial<LiveTrafficFiltersState>) => void;
  source: TrafficSourceFilter;
}

export function LiveTrafficAdvancedFilters({
  value: v,
  onPatch,
  source,
}: LiveTrafficAdvancedFiltersProps) {
  const { t } = useTranslation();

  if (source === '') return null;

  if (source === 'vk') {
    return (
      <div className={css.advPanel}>
        {/* HTTP / Cache */}
        <div className={css.advGroupTitleFirst}>{t('pages:traffic.httpCacheTitle')}</div>
        <div className={css.advInnerGrid}>
          <FieldCompact label={t('pages:traffic.labelStatusClass')} tip={t('pages:traffic.tipStatusClass')}>
            <select
              aria-label={t('pages:traffic.labelStatusClass')}
              value={v.statusRange}
              onChange={(e) => onPatch({ statusRange: e.target.value as LiveTrafficFiltersState['statusRange'] })}
              className={css.nativeSelect}
            >
              <option value="">{t('pages:traffic.optionAny')}</option>
              <option value="2xx">{t('pages:traffic.option2xx')}</option>
              <option value="4xx">{t('pages:traffic.option4xx')}</option>
              <option value="5xx">{t('pages:traffic.option5xx')}</option>
            </select>
          </FieldCompact>
          <FieldCompact label={t('pages:traffic.labelStatusCode')} tip={t('pages:traffic.tipStatusCode')}>
            <Input
              type="text"
              inputMode="numeric"
              aria-label={t('pages:traffic.labelStatusCode')}
              value={v.statusCode}
              onChange={(e) => onPatch({ statusCode: e.target.value.replace(/\D/g, '').slice(0, 3) })}
              placeholder={t('pages:traffic.placeholderStatusCode')}
              className={css.dtInput}
            />
          </FieldCompact>
          <FieldCompact label={t('pages:traffic.labelCache')} tip={t('pages:traffic.tipCache')}>
            <select
              aria-label={t('pages:traffic.labelCache')}
              value={v.cacheStatus}
              onChange={(e) => onPatch({ cacheStatus: e.target.value as LiveTrafficFiltersState['cacheStatus'] })}
              className={css.nativeSelect}
            >
              <option value="">{t('pages:traffic.optionAny')}</option>
              <option value="HIT">{t('pages:traffic.cacheStatus.HIT')}</option>
              <option value="MISS">{t('pages:traffic.cacheStatus.MISS')}</option>
            </select>
          </FieldCompact>
        </div>

        {/* Correlation */}
        <div className={css.advGroupTitle}>{t('pages:traffic.correlationTitle')}</div>
        <FieldCompact label={t('pages:traffic.labelGatewayRequestId')} tip={t('pages:traffic.tipGatewayRequestId')}>
          <Input
            type="text"
            aria-label={t('pages:traffic.labelGatewayRequestId')}
            value={v.requestId}
            onChange={(e) => onPatch({ requestId: e.target.value })}
            placeholder={t('pages:traffic.placeholderRequestId')}
            className={css.dtInputMono}
          />
        </FieldCompact>
      </div>
    );
  }

  if (source === 'proxy') {
    return (
      <div className={css.advPanel}>
        {/* Compliance */}
        <div className={css.advGroupTitleFirst}>{t('pages:traffic.complianceGroupTitle')}</div>
        <div className={css.advInnerGrid}>
          <FieldCompact
            label={t('pages:traffic.filters.complianceTagFilter')}
            tip={t('pages:traffic.filters.complianceTagPlaceholder')}
          >
            <ComplianceTagChipInput
              value={v.complianceTags}
              onChange={(tags) => onPatch({ complianceTags: tags })}
              placeholder={t('pages:traffic.filters.complianceTagPlaceholder')}
              ariaLabel={t('pages:traffic.filters.complianceTagFilter')}
            />
          </FieldCompact>
          <FieldCompact label={t('pages:traffic.labelResponseHook')} tip={t('pages:traffic.tipResponseHook')}>
            <select
              aria-label={t('pages:traffic.labelResponseHook')}
              value={v.responseHookDecision}
              onChange={(e) => onPatch({ responseHookDecision: e.target.value })}
              className={css.nativeSelect}
            >
              <option value="">{t('pages:traffic.optionAll')}</option>
              {HOOK_OPTIONS.map((h) => (
                <option key={h} value={h}>{h}</option>
              ))}
            </select>
          </FieldCompact>
        </div>

        {/* HTTP */}
        <div className={css.advGroupTitle}>{t('pages:traffic.httpCacheTitle')}</div>
        <div className={css.advInnerGrid}>
          <FieldCompact label={t('pages:traffic.labelStatusClass')} tip={t('pages:traffic.tipStatusClass')}>
            <select
              aria-label={t('pages:traffic.labelStatusClass')}
              value={v.statusRange}
              onChange={(e) => onPatch({ statusRange: e.target.value as LiveTrafficFiltersState['statusRange'] })}
              className={css.nativeSelect}
            >
              <option value="">{t('pages:traffic.optionAny')}</option>
              <option value="2xx">{t('pages:traffic.option2xx')}</option>
              <option value="4xx">{t('pages:traffic.option4xx')}</option>
              <option value="5xx">{t('pages:traffic.option5xx')}</option>
            </select>
          </FieldCompact>
          <FieldCompact label={t('pages:traffic.labelStatusCode')} tip={t('pages:traffic.tipStatusCode')}>
            <Input
              type="text"
              inputMode="numeric"
              aria-label={t('pages:traffic.labelStatusCode')}
              value={v.statusCode}
              onChange={(e) => onPatch({ statusCode: e.target.value.replace(/\D/g, '').slice(0, 3) })}
              placeholder={t('pages:traffic.placeholderStatusCode')}
              className={css.dtInput}
            />
          </FieldCompact>
        </div>
      </div>
    );
  }

  if (source === 'agent') {
    return (
      <div className={css.advPanel}>
        {/* Compliance */}
        <div className={css.advGroupTitleFirst}>{t('pages:traffic.complianceGroupTitle')}</div>
        <div className={css.advInnerGrid}>
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
          <FieldCompact
            label={t('pages:traffic.filters.complianceTagFilter')}
            tip={t('pages:traffic.filters.complianceTagPlaceholder')}
          >
            <ComplianceTagChipInput
              value={v.complianceTags}
              onChange={(tags) => onPatch({ complianceTags: tags })}
              placeholder={t('pages:traffic.filters.complianceTagPlaceholder')}
              ariaLabel={t('pages:traffic.filters.complianceTagFilter')}
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
        </div>
      </div>
    );
  }

  return null;
}
