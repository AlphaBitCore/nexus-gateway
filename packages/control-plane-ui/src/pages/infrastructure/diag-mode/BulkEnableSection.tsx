import { useTranslation } from 'react-i18next';
import {
  Stack, Card, Button, Input, Textarea, Select,
  ErrorBanner, FormField,
} from '@/components/ui';
import { MAX_BULK_THINGS, WINDOW_OPTIONS, ANY_VALUE } from './diagModeHelpers';
import styles from './InfraDiagModePage.module.css';

interface BulkEnableSectionProps {
  agentVersion: string;
  setAgentVersion: (v: string) => void;
  os: string;
  setOs: (v: string) => void;
  thingIdsRaw: string;
  setThingIdsRaw: (v: string) => void;
  windowKey: string;
  setWindowKey: (v: string) => void;
  reason: string;
  setReason: (v: string) => void;
  parsedThingIds: string[];
  previewCount: number | null;
  previewLoading: boolean;
  previewError: string | null;
  handlePreview: () => void;
  validReason: boolean;
  canSubmit: boolean;
  bulkLoading: boolean;
  handleBulkEnable: () => void;
}

export function BulkEnableSection({
  agentVersion,
  setAgentVersion,
  os,
  setOs,
  thingIdsRaw,
  setThingIdsRaw,
  windowKey,
  setWindowKey,
  reason,
  setReason,
  parsedThingIds,
  previewCount,
  previewLoading,
  previewError,
  handlePreview,
  validReason,
  canSubmit,
  bulkLoading,
  handleBulkEnable,
}: BulkEnableSectionProps) {
  const { t } = useTranslation('pages');

  return (
    /* ── Bulk enable ── */
    <section className={styles.contentSection}>
      <h3 className={styles.sectionTitle}>{t('infrastructure.diagMode.bulkTitle')}</h3>
      <Card>
        <Stack gap="md">
        <div className={styles.formGrid}>
          <FormField
            label={t('infrastructure.diagMode.filterAgentVersion')}
            helpText={t('infrastructure.diagMode.filterAgentVersionHelp')}
            className={styles.formHalf}
          >
            <Input
              type="text"
              placeholder={t('infrastructure.diagMode.filterAgentVersionPlaceholder')}
              value={agentVersion}
              onChange={(e) => setAgentVersion(e.target.value)}
              disabled={parsedThingIds.length > 0}
            />
          </FormField>

          <FormField
            label={t('infrastructure.diagMode.filterOs')}
            helpText={t('infrastructure.diagMode.filterOsHelp')}
            className={styles.formHalf}
          >
            <Select
              value={os || ANY_VALUE}
              onValueChange={(v) => setOs(v === ANY_VALUE ? '' : v)}
              disabled={parsedThingIds.length > 0}
              options={[
                { value: ANY_VALUE, label: t('infrastructure.diagMode.osAny') },
                { value: 'darwin', label: t('infrastructure.diagMode.osDarwin', 'macOS (darwin)') },
                { value: 'linux', label: t('infrastructure.diagMode.osLinux', 'Linux') },
                { value: 'windows', label: t('infrastructure.diagMode.osWindows', 'Windows') },
              ]}
            />
          </FormField>

          <FormField
            label={t('infrastructure.diagMode.filterThingIds')}
            helpText={t('infrastructure.diagMode.filterThingIdsHelp')}
            className={styles.formGridFull}
          >
            <Textarea
              rows={3}
              className={styles.nodeIdsTextarea}
              placeholder={t('infrastructure.diagMode.filterThingIdsPlaceholder')}
              value={thingIdsRaw}
              onChange={(e) => setThingIdsRaw(e.target.value)}
            />
          </FormField>

          <FormField label={t('infrastructure.diagMode.window')} className={styles.windowField}>
            <Select
              value={windowKey}
              onValueChange={setWindowKey}
              options={WINDOW_OPTIONS.map((w) => ({
                value: w.value,
                label: t(`infrastructure.diagMode.window_${w.value}`),
              }))}
            />
          </FormField>

          <FormField
            label={t('infrastructure.diagMode.reason')}
            required
            error={!validReason && reason.length > 0 ? t('infrastructure.diagMode.reasonRequired') : undefined}
            className={styles.reasonField}
          >
            <Input
              type="text"
              placeholder={t('infrastructure.diagMode.reasonPlaceholder')}
              value={reason}
              onChange={(e) => setReason(e.target.value)}
            />
          </FormField>
        </div>

        {/* Preview row */}
        <Stack gap="xs">
          <div className={styles.actionRow}>
            <Button
              type="button"
              variant="secondary"
              size="sm"
              loading={previewLoading}
              onClick={handlePreview}
            >
              {t('infrastructure.diagMode.previewButton')}
            </Button>
            {previewCount !== null && (
              <span className={styles.previewBanner}>
                {previewCount === 0 ? (
                  t('infrastructure.diagMode.previewEmpty')
                ) : (
                  <>
                    <span className={styles.previewCount}>
                      {t('infrastructure.diagMode.previewCount', { count: previewCount })}
                    </span>
                    {previewCount > MAX_BULK_THINGS && (
                      <span className={styles.validationError} style={{ marginLeft: 'var(--g-space-2)' }}>
                        {t('infrastructure.diagMode.previewOverLimit', { max: MAX_BULK_THINGS })}
                      </span>
                    )}
                  </>
                )}
              </span>
            )}
          </div>
          {previewError && <ErrorBanner message={previewError} />}
        </Stack>

        <div className={styles.actionRow}>
          <Button
            type="button"
            variant="primary"
            size="md"
            className={styles.enableButton}
            disabled={!canSubmit}
            loading={bulkLoading}
            onClick={handleBulkEnable}
          >
            {t('infrastructure.diagMode.bulkSubmit')}
          </Button>
          {previewCount === null && (
            <span className={styles.previewBanner}>
              {t('infrastructure.diagMode.bulkPreviewFirst')}
            </span>
          )}
        </div>
        </Stack>
      </Card>
    </section>
  );
}
