import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import styles from './SettingsStreamingComplianceTab.module.css';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import {
  systemApi,
  type StreamingComplianceConfig,
} from '@/api/services/infrastructure/misc/system';
import {
  Button,
  Card,
  ErrorBanner,
  FormField,
  Input,
  Skeleton,
  Stack,
  Switch,
} from '@/components/ui';

// SettingsStreamingComplianceTab edits the global StreamingPolicy default
// stored in system_metadata['streaming_compliance.config']. Per-host /
// per-provider overrides live on interception_domain (compliance-proxy +
// agent) and Provider (ai-gateway) and reuse the existing edit panels for
// those resources — this tab only owns the global default knob.
//
// Mode legend (see the in-panel disclosure for the user-facing wording):
//   passthrough        — relay only; compliance scans at storage time, no
//                        inflight redaction. Lowest latency.
//   buffer_full_block  — hold the full response, scan before any byte ships;
//                        redaction + hard-block guaranteed. Higher TTFB.
//   chunked_async      — real-time stream with best-effort inflight redaction;
//                        bounded-fragment wire risk (a short leading fragment
//                        may reach the client before redaction engages).
export function SettingsStreamingComplianceTab() {
  const { t } = useTranslation();

  const [defaultMode, setDefaultMode] = useState<StreamingComplianceConfig['default_mode']>('passthrough');
  const [chunkBytes, setChunkBytes] = useState('8192');
  const [hookTimeoutMs, setHookTimeoutMs] = useState('2000');
  const [maxBufferBytes, setMaxBufferBytes] = useState('67108864');
  const [failBehavior, setFailBehavior] = useState<StreamingComplianceConfig['fail_behavior']>('fail_open');
  const [captureRequest, setCaptureRequest] = useState(false);
  const [captureResponse, setCaptureResponse] = useState(false);
  const [rawSpillEnabled, setRawSpillEnabled] = useState(false);

  const { data, loading, error, refetch } = useApi<StreamingComplianceConfig>(
    () => systemApi.getStreamingComplianceConfig(),
    ['admin', 'settings', 'streaming-compliance'],
  );

  useEffect(() => {
    if (!data) return;
    setDefaultMode(data.default_mode);
    setChunkBytes(String(data.chunk_bytes));
    setHookTimeoutMs(String(data.hook_timeout_ms));
    setMaxBufferBytes(String(data.max_buffer_bytes));
    setFailBehavior(data.fail_behavior);
    setCaptureRequest(!!data.capture_request_body);
    setCaptureResponse(!!data.capture_response_body);
    setRawSpillEnabled(!!data.raw_body_spill_enabled);
  }, [data]);

  const { mutate: save, loading: saving } = useMutation(
    () =>
      systemApi.updateStreamingComplianceConfig({
        default_mode: defaultMode,
        chunk_bytes: Number.parseInt(chunkBytes, 10) || 0,
        hook_timeout_ms: Number.parseInt(hookTimeoutMs, 10) || 0,
        max_buffer_bytes: Number.parseInt(maxBufferBytes, 10) || 0,
        fail_behavior: failBehavior,
        capture_request_body: captureRequest,
        capture_response_body: captureResponse,
        raw_body_spill_enabled: rawSpillEnabled,
      }),
    {
      invalidateQueries: [['admin', 'settings', 'streaming-compliance']],
      onSuccess: () => refetch(),
    },
  );

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!data) return null;

  return (
    <Stack gap="md">
      <div className={styles.sectionHeader}>
        <h2 className={styles.sectionTitle}>{t('pages:settingsStreamingCompliance.title', 'Streaming Compliance')}</h2>
        <p className={styles.sectionSubtitle}>
          {t(
            'pages:settingsStreamingCompliance.subtitle',
            'Global default for SSE response handling. Per-host overrides live on Interception Domains; per-provider overrides live on Providers.',
          )}
        </p>
      </div>
      <Card>
        <Stack gap="md">
          <div className={styles.formGrid}>
            <FormField
              label={t('pages:settingsStreamingCompliance.defaultMode', 'Default Mode')}
              helpText={t(
                'pages:settingsStreamingCompliance.defaultModeHelp',
                'Stream mode is high-performance but best-effort on the wire; for guaranteed redaction/blocking choose Buffer.',
              )}
            >
              <select
                value={defaultMode}
                onChange={(e) => setDefaultMode(e.target.value as StreamingComplianceConfig['default_mode'])}
                className={styles.nativeSelect}
              >
                <option value="passthrough">passthrough</option>
                <option value="buffer_full_block">buffer_full_block</option>
                <option value="chunked_async">chunked_async</option>
              </select>
            </FormField>

            <FormField
              label={t('pages:settingsStreamingCompliance.chunkBytes', 'Chunk Bytes')}
              helpText={t(
                'pages:settingsStreamingCompliance.chunkBytesHelp',
                'chunked_async: bytes scanned per checkpoint. Adapts upward for large responses.',
              )}
            >
              <Input type="number" value={chunkBytes} onChange={(e) => setChunkBytes(e.target.value)} min={0} step={1024} />
            </FormField>
            <FormField
              label={t('pages:settingsStreamingCompliance.hookTimeoutMs', 'Hook Timeout (ms)')}
              helpText={t(
                'pages:settingsStreamingCompliance.hookTimeoutMsHelp',
                'Per-hook execution budget. Exceeding the budget triggers fail_behavior.',
              )}
            >
              <Input type="number" value={hookTimeoutMs} onChange={(e) => setHookTimeoutMs(e.target.value)} min={0} step={100} />
            </FormField>
            <FormField
              label={t('pages:settingsStreamingCompliance.maxBufferBytes', 'Max Buffer (bytes)')}
              helpText={t(
                'pages:settingsStreamingCompliance.maxBufferBytesHelp',
                'Per-stream in-memory cap. Streams over this threshold spill to SpillStore (when enabled) or are truncated.',
              )}
            >
              <Input type="number" value={maxBufferBytes} onChange={(e) => setMaxBufferBytes(e.target.value)} min={0} step={1024 * 1024} />
            </FormField>

            <FormField
              label={t('pages:settingsStreamingCompliance.failBehavior', 'On Hook Failure')}
              helpText={t(
                'pages:settingsStreamingCompliance.failBehaviorHelp',
                'fail_open: continue on hook error/timeout. fail_close: block (buffer_full_block) or audit-flag (chunked_async).',
              )}
            >
              <select
                value={failBehavior}
                onChange={(e) => setFailBehavior(e.target.value as StreamingComplianceConfig['fail_behavior'])}
                className={styles.nativeSelect}
              >
                <option value="fail_open">fail_open</option>
                <option value="fail_close">fail_close</option>
              </select>
            </FormField>
          </div>

        {/*
          Honest per-mode disclosure (user-binding). One plain sentence per
          mode stating exactly what compliance enforcement it does — and, for
          stream mode, the bounded-fragment wire risk admins MUST see before
          choosing it over Buffer. Informational, always visible; not a knob.
        */}
        <div role="note" className={styles.disclosure}>
          <div className={styles.disclosureTitle}>
            {t('pages:settingsStreamingCompliance.disclosureTitle', 'How each mode handles compliance')}
          </div>
          <div className={styles.disclosureRow}>
            <span className={styles.disclosureMode}>passthrough</span>
            <span>
              {t(
                'pages:settingsStreamingCompliance.disclosurePassthrough',
                'Streamed in real time; compliance scanning runs at storage time only — no inflight redaction. Lowest latency, no inflight enforcement.',
              )}
            </span>
          </div>
          <div className={styles.disclosureRow}>
            <span className={styles.disclosureMode}>chunked_async</span>
            <span>
              {t(
                'pages:settingsStreamingCompliance.disclosureStream',
                'Real-time streaming with inflight redaction on a best-effort basis. High performance, but carries a bounded-fragment risk: a complete sensitive value is never delivered, yet a short leading fragment may reach the client before redaction engages. Choose buffer_full_block for guaranteed redaction.',
              )}
            </span>
          </div>
          <div className={styles.disclosureRow}>
            <span className={styles.disclosureMode}>buffer_full_block</span>
            <span>
              {t(
                'pages:settingsStreamingCompliance.disclosureBuffer',
                'The full response is buffered and scanned before any byte is delivered — redaction and hard-block are guaranteed (strong compliance), at the cost of higher time-to-first-byte.',
              )}
            </span>
          </div>
        </div>

        <div className={styles.switchGrid}>
          <div className={styles.switchField}>
            <div className={styles.switchTitle}>{t('pages:settingsStreamingCompliance.captureRequestTitle', 'Capture request body')}</div>
            <div className={styles.switchRow}>
              <Switch checked={captureRequest} onCheckedChange={setCaptureRequest} aria-label="capture request body" />
              <span className={styles.switchHelp}>{t('pages:settingsStreamingCompliance.captureRequestHelp', '(default; per-host can override)')}</span>
            </div>
          </div>

          <div className={styles.switchField}>
            <div className={styles.switchTitle}>{t('pages:settingsStreamingCompliance.captureResponseTitle', 'Capture response body')}</div>
            <div className={styles.switchRow}>
              <Switch checked={captureResponse} onCheckedChange={setCaptureResponse} aria-label="capture response body" />
              <span className={styles.switchHelp}>{t('pages:settingsStreamingCompliance.captureResponseHelp', '(default; per-host can override)')}</span>
            </div>
          </div>

          <div className={styles.switchField}>
            <div className={styles.switchTitle}>{t('pages:settingsStreamingCompliance.rawSpillEnabledTitle', 'Spill bodies larger than the inline threshold to SpillStore')}</div>
            <div className={styles.switchRow}>
              <Switch checked={rawSpillEnabled} onCheckedChange={setRawSpillEnabled} aria-label="enable raw body spill" />
              <span className={styles.switchHelp}>{t('pages:settingsStreamingCompliance.rawSpillEnabledHelpShort', '(default: localfs).')}</span>
            </div>
          </div>
        </div>

        <Stack direction="horizontal" gap="sm">
          <Button className={styles.saveButton} onClick={() => save(undefined)} loading={saving}>
            {t('common:save')}
          </Button>
        </Stack>
        </Stack>
      </Card>
    </Stack>
  );
}
