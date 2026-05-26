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
// Mode legend:
//   passthrough        — relay only, no hook, no body capture.
//   buffer_full_block  — hold full response, run hook at end, can HTTP-451.
//                        Trades real-time UX for the ability to block.
//   chunked_async      — relay bytes in real time + accumulate + run hook
//                        per chunk. Audit-only; cannot stop sent bytes.
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
    <Card>
      <Stack gap="md">
        <h2>{t('pages:settingsStreamingCompliance.title', 'Streaming Compliance')}</h2>
        <p style={{ fontSize: 'var(--g-font-size-base)', color: 'var(--color-text-secondary)' }}>
          {t(
            'pages:settingsStreamingCompliance.subtitle',
            'Global default for SSE response handling. Per-host overrides live on Interception Domains; per-provider overrides live on Providers.',
          )}
        </p>

        <FormField
          label={t('pages:settingsStreamingCompliance.defaultMode', 'Default Mode')}
          helpText={t(
            'pages:settingsStreamingCompliance.defaultModeHelp',
            'passthrough: relay only. buffer_full_block: hold response + run hook + can block (breaks SSE realtime). chunked_async: real-time relay + audit-only hook.',
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

        {/*
          Mode advisories. Two sources:
          (a) data.warnings — what the backend returned for the persisted
              mode. Single source of truth — modeWarnings() in Go.
          (b) localModeWarning — fired when admin has picked a different
              mode in the dropdown but hasn't saved yet. Same text the
              backend would return; surfacing pre-save so they aren't
              surprised after Save.
          We render unconditionally when present (no extra hover/tooltip
          chrome) — these are constraints admins MUST see, not optional
          help. Style follows the existing subtitle muted text.
        */}
        {(() => {
          const persistedWarnings = data?.warnings ?? [];
          const localWarning =
            defaultMode === 'buffer_full_block' &&
            data?.default_mode !== 'buffer_full_block'
              ? t(
                  'pages:settingsStreamingCompliance.bufferModifyWarning',
                  'Heads up: buffer_full_block silently ignores response-hook Modify decisions (the original body replays unchanged). Use chunked_async if rewrite is required.',
                )
              : null;
          const lines = [...persistedWarnings];
          if (localWarning) lines.push(localWarning);
          if (lines.length === 0) return null;
          return (
            <div
              role="note"
              style={{
                padding: 'var(--g-space-3)',
                border: '1px solid var(--color-border-warning, var(--color-border))',
                borderRadius: 'var(--g-radius-sm)',
                background: 'var(--color-background-warning-subtle, transparent)',
                fontSize: 'var(--g-font-size-sm)',
                color: 'var(--color-text-secondary)',
              }}
            >
              {lines.map((w, i) => (
                <div key={i}>{w}</div>
              ))}
            </div>
          );
        })()}

        {/*
          Three numeric knobs side-by-side. CSS Grid (vs the previous Stack
          + flex:1 wrappers) gives every cell the same width and a single
          top-aligned baseline so the inputs don't bob up/down when the
          help text under each label wraps to a different number of lines.
        */}
        <div
          style={{
            display: 'grid',
            gridTemplateColumns: '1fr 1fr 1fr',
            columnGap: 'var(--g-space-4)',
            alignItems: 'flex-start',
          }}
        >
          <FormField
            label={t('pages:settingsStreamingCompliance.chunkBytes', 'Chunk Bytes')}
            helpText={t(
              'pages:settingsStreamingCompliance.chunkBytesHelp',
              'chunked_async: bytes per checkpoint. Adapts upward when total/chunk_bytes > 64.',
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
        </div>

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

        <Stack direction="horizontal" gap="sm" style={{ alignItems: 'center' }}>
          <Switch checked={captureRequest} onCheckedChange={setCaptureRequest} aria-label="capture request body" />
          <span style={{ fontSize: 'var(--g-font-size-base)' }}>
            {t('pages:settingsStreamingCompliance.captureRequest', 'Capture request body (default; per-host can override)')}
          </span>
        </Stack>

        <Stack direction="horizontal" gap="sm" style={{ alignItems: 'center' }}>
          <Switch checked={captureResponse} onCheckedChange={setCaptureResponse} aria-label="capture response body" />
          <span style={{ fontSize: 'var(--g-font-size-base)' }}>
            {t('pages:settingsStreamingCompliance.captureResponse', 'Capture response body (default; per-host can override)')}
          </span>
        </Stack>

        <Stack direction="horizontal" gap="sm" style={{ alignItems: 'center' }}>
          <Switch checked={rawSpillEnabled} onCheckedChange={setRawSpillEnabled} aria-label="enable raw body spill" />
          <span style={{ fontSize: 'var(--g-font-size-base)' }}>
            {t(
              'pages:settingsStreamingCompliance.rawSpillEnabled',
              'Spill bodies larger than the inline threshold to SpillStore (default: localfs).',
            )}
          </span>
        </Stack>

        <Stack direction="horizontal" gap="sm">
          <Button onClick={() => save(undefined)} loading={saving}>
            {t('common:save')}
          </Button>
        </Stack>
      </Stack>
    </Card>
  );
}
