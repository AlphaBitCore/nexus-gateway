import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { rulePacksApi, type RulePackImportResult, type RulePackPreviewResult } from '@/api/services';
import { Button, Dialog, ErrorBanner, FormField, Stack, Textarea } from '@/components/ui';
import { useMutation } from '@/hooks/useMutation';

import styles from './ImportPackModal.module.css';

export interface ImportPackModalProps {
  open: boolean;
  onClose: () => void;
}

export function ImportPackModal({ open, onClose }: ImportPackModalProps) {
  const { t } = useTranslation();
  const [yaml, setYaml] = useState('');
  const [preview, setPreview] = useState<RulePackPreviewResult | null>(null);
  const [previewError, setPreviewError] = useState<string | null>(null);
  const [previewing, setPreviewing] = useState(false);

  const { mutate: importPack, loading: importing } = useMutation<string, RulePackImportResult>(
    (body) => rulePacksApi.import(body),
    {
      invalidateQueries: [['admin', 'rule-packs', 'list']],
      successMessage: t('pages:hooks.rulePacks.importSuccess', 'Rule pack imported'),
      onSuccess: () => {
        handleClose();
      },
    },
  );

  function handleClose() {
    setYaml('');
    setPreview(null);
    setPreviewError(null);
    onClose();
  }

  async function onPreview() {
    setPreviewing(true);
    setPreviewError(null);
    try {
      const result = await rulePacksApi.preview(yaml);
      setPreview(result);
    } catch (err) {
      setPreview(null);
      setPreviewError(err instanceof Error ? err.message : String(err));
    } finally {
      setPreviewing(false);
    }
  }

  async function onImport() {
    await importPack(yaml);
  }

  const canImport = yaml.trim() !== '' && preview !== null && preview.errors.length === 0;

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) handleClose();
      }}
      title={t('pages:hooks.rulePacks.importTitle', 'Import Rule Pack')}
      description={t(
        'pages:hooks.rulePacks.importSubtitle',
        'Paste authored YAML, preview validation results, then import the pack into the catalog.',
      )}
      size="lg"
    >
      <Stack gap="md">
        <FormField label={t('pages:hooks.rulePacks.importYaml', 'YAML')}>
          <Textarea
            aria-label={t('pages:hooks.rulePacks.importYaml', 'YAML')}
            className={styles.textarea}
            value={yaml}
            rows={16}
            onChange={(e) => setYaml(e.target.value)}
            placeholder={t(
              'pages:hooks.rulePacks.importPlaceholder',
              'name: acme/test\nversion: v1.0.0\nmaintainer: customer\nrules: []',
            )}
          />
        </FormField>

        <div className={styles.actions}>
          <Button variant="secondary" onClick={onPreview} loading={previewing} disabled={yaml.trim() === ''}>
            {t('pages:hooks.rulePacks.importPreview', 'Preview')}
          </Button>
          <Button onClick={onImport} loading={importing} disabled={!canImport}>
            {t('pages:hooks.rulePacks.importButton', 'Import')}
          </Button>
        </div>

        {previewError && <ErrorBanner message={previewError} />}

        {preview && (
          <div className={styles.preview}>
            {preview.pack && (
              <div className={styles.summary}>
                <strong>{preview.pack.name}</strong>
                <span>{preview.pack.version}</span>
              </div>
            )}

            {preview.warnings.length > 0 && (
              <div>
                <div className={styles.listTitle}>
                  {t('pages:hooks.rulePacks.importWarnings', 'Warnings')}
                </div>
                <ul className={styles.list}>
                  {preview.warnings.map((warning) => (
                    <li key={warning}>{warning}</li>
                  ))}
                </ul>
              </div>
            )}

            {preview.errors.length > 0 && (
              <div>
                <div className={styles.errorTitle}>
                  {t('pages:hooks.rulePacks.importErrors', 'Errors')}
                </div>
                <ul className={styles.list}>
                  {preview.errors.map((item) => (
                    <li key={item}>{item}</li>
                  ))}
                </ul>
              </div>
            )}

            {preview.lint && preview.lint.length > 0 && (
              <div>
                <div className={styles.listTitle}>
                  {t(
                    'pages:hooks.rulePacks.importLintTitle',
                    'Engine compatibility & performance',
                  )}
                </div>
                <ul className={styles.list}>
                  {preview.lint.flatMap((rule) =>
                    rule.findings.map((finding) => (
                      <li key={`${rule.ruleId}:${finding.code}`}>
                        <code>{rule.ruleId}</code> — {finding.message}
                      </li>
                    )),
                  )}
                </ul>
              </div>
            )}
          </div>
        )}
      </Stack>
    </Dialog>
  );
}

