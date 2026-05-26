import { useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Button,
  Checkbox,
  Dialog,
  ErrorBanner,
  FormField,
  Input,
  PageHeader,
  Popover,
  PopoverContent,
  PopoverTrigger,
  Select,
  Stack,
  Switch,
  Textarea,
} from '@/components/ui';
import {
  aiGatewayClientSimulatorApi,
  validateRequest,
  type RequestFormat,
  type RequestParams,
  type SimulatorChatMessage,
  type SimulatorCompletionUsage,
  type SimulatorUsageSummaryResponse,
} from '@/api/services/ai-gateway/aiGatewayClientSimulator';
import styles from './AIGatewaySimulatorPage.module.css';

interface ProviderModelOption {
  id: string;
  label: string;
}

interface ProviderGroup {
  providerKey: string;
  providerLabel: string;
  models: ProviderModelOption[];
}

function groupModelsByProvider(
  models: Array<{ id: string; name?: string; owned_by?: string; owner_display_name?: string }>,
): ProviderGroup[] {
  const byProvider = new Map<string, { label: string; models: Map<string, string> }>();
  for (const model of models) {
    const providerKey = model.owned_by?.trim() || 'unknown';
    const providerLabel = model.owner_display_name?.trim() || providerKey;
    const current = byProvider.get(providerKey) ?? { label: providerLabel, models: new Map<string, string>() };
    const label = model.name?.trim() || model.id;
    current.models.set(model.id, label);
    if (model.owner_display_name?.trim()) current.label = model.owner_display_name.trim();
    byProvider.set(providerKey, current);
  }
  return Array.from(byProvider.entries())
    .map(([providerKey, value]) => ({
      providerKey,
      providerLabel: value.label,
      models: Array.from(value.models.entries())
        .map(([id, label]) => ({ id, label }))
        .sort((a, b) => a.label.localeCompare(b.label)),
    }))
    .sort((a, b) => a.providerLabel.localeCompare(b.providerLabel));
}

type StandardParamKey =
  | 'temperature'
  | 'max_tokens'
  | 'top_p'
  | 'presence_penalty'
  | 'frequency_penalty'
  | 'seed'
  | 'stop'
  | 'system';

type ParamKind = 'number' | 'integer' | 'text';

interface StandardParamMeta {
  kind: ParamKind;
  defaultValue: string;
}

interface ParamRowState {
  enabled: boolean;
  value: string;
}

const STANDARD_PARAM_META: Record<StandardParamKey, StandardParamMeta> = {
  temperature: { kind: 'number', defaultValue: '1.0' },
  max_tokens: { kind: 'integer', defaultValue: '1024' },
  top_p: { kind: 'number', defaultValue: '1.0' },
  presence_penalty: { kind: 'number', defaultValue: '0' },
  frequency_penalty: { kind: 'number', defaultValue: '0' },
  seed: { kind: 'integer', defaultValue: '' },
  stop: { kind: 'text', defaultValue: '' },
  system: { kind: 'text', defaultValue: '' },
};

const STANDARD_PARAM_ORDER: StandardParamKey[] = [
  'temperature',
  'max_tokens',
  'top_p',
  'presence_penalty',
  'frequency_penalty',
  'seed',
  'stop',
  'system',
];

function makeInitialParams(): Record<StandardParamKey, ParamRowState> {
  const out = {} as Record<StandardParamKey, ParamRowState>;
  for (const k of STANDARD_PARAM_ORDER) {
    out[k] = { enabled: false, value: STANDARD_PARAM_META[k].defaultValue };
  }
  return out;
}

interface CustomParam {
  /** Stable id for React keys. Generated client-side; never sent to server. */
  id: string;
  enabled: boolean;
  key: string;
  /** User-typed string. Parsed as JSON at send time when possible — so
   * `{"type":"enabled","budget_tokens":2000}` lands as a nested object,
   * but plain strings/numbers still work. */
  value: string;
}

function newCustomId(): string {
  return Math.random().toString(36).slice(2, 10);
}

/** Best-effort: try JSON.parse(value) so an object/array/number reaches
 * the wire body as a structured type, otherwise pass the raw string. */
function parseCustomValue(raw: string): unknown {
  const trimmed = raw.trim();
  if (trimmed === '') return '';
  try {
    return JSON.parse(trimmed);
  } catch {
    return raw;
  }
}

/** Translate the UI's row-by-row param state into the wire-body
 * RequestParams shape, omitting any row whose checkbox is off. Numeric
 * values fall back to the default when the operator typed garbage —
 * the error path then surfaces from the upstream API rather than from a
 * silent client-side coercion. */
function buildRequestParams(
  std: Record<StandardParamKey, ParamRowState>,
  custom: CustomParam[],
): RequestParams {
  const out: RequestParams = {};
  for (const k of STANDARD_PARAM_ORDER) {
    const row = std[k];
    if (!row.enabled) continue;
    const meta = STANDARD_PARAM_META[k];
    if (meta.kind === 'number' || meta.kind === 'integer') {
      const n = meta.kind === 'integer' ? Number.parseInt(row.value, 10) : Number(row.value);
      if (Number.isFinite(n)) {
        (out as Record<string, unknown>)[k] = n;
      }
    } else {
      if (row.value.length > 0) {
        (out as Record<string, unknown>)[k] = row.value;
      }
    }
  }
  if (custom.length > 0) {
    const cp: Record<string, unknown> = {};
    for (const row of custom) {
      if (!row.enabled || row.key.trim() === '') continue;
      cp[row.key.trim()] = parseCustomValue(row.value);
    }
    if (Object.keys(cp).length > 0) out.customParams = cp;
  }
  return out;
}

const FORMAT_OPTIONS: Array<{ value: RequestFormat; label: string }> = [
  { value: 'openai', label: 'OpenAI Chat (/v1/chat/completions)' },
  { value: 'openai-responses', label: 'OpenAI Responses (/v1/responses)' },
  { value: 'anthropic', label: 'Anthropic Messages (/v1/messages)' },
  { value: 'gemini', label: 'Gemini (/v1beta/.../:generateContent)' },
];

export function AIGatewaySimulatorPage() {
  const { t } = useTranslation();
  // Connection — gateway URL is resolved server-side (CP knows where
  // ai-gateway is, sourced from env AI_GATEWAY_URL with a localhost
  // fallback). UI only needs the operator's VK.
  const gatewayBaseUrl = '';
  const [vk, setVk] = useState('');
  const [providerGroups, setProviderGroups] = useState<ProviderGroup[]>([]);
  const [selectedProvider, setSelectedProvider] = useState('');
  const [selectedModel, setSelectedModel] = useState('');
  const [stream, setStream] = useState(false);
  const [format, setFormat] = useState<RequestFormat>('openai');
  // Per-request knobs: each standard param has its own checkbox+value
  // row, plus an open-ended list of custom params for provider-specific
  // extensions the simulator doesn't surface as first-class checkboxes.
  // Default state: every checkbox off so requests land at the upstream
  // exactly the way the model defaults expect — flipping a model from
  // OpenAI to Claude doesn't carry over a stale `temperature: 0.7` that
  // the new model would reject.
  const [stdParams, setStdParams] = useState<Record<StandardParamKey, ParamRowState>>(() =>
    makeInitialParams(),
  );
  const [customParams, setCustomParams] = useState<CustomParam[]>([]);
  // Chat state.
  const [inputText, setInputText] = useState('');
  const [messages, setMessages] = useState<SimulatorChatMessage[]>([]);
  const [lastUsage, setLastUsage] = useState<SimulatorCompletionUsage | null>(null);
  const [usageSummary, setUsageSummary] = useState<SimulatorUsageSummaryResponse | null>(null);
  const [streamingAssistant, setStreamingAssistant] = useState('');
  const [streamController, setStreamController] = useState<AbortController | null>(null);
  // UX state.
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [paramsOpen, setParamsOpen] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [loadingModels, setLoadingModels] = useState(false);
  const [requestError, setRequestError] = useState<string | null>(null);
  const [sending, setSending] = useState(false);

  const providerOptions = useMemo(
    () => providerGroups.map((g) => ({ value: g.providerKey, label: g.providerLabel })),
    [providerGroups],
  );
  const modelOptions = useMemo(() => {
    const group = providerGroups.find((g) => g.providerKey === selectedProvider);
    return (group?.models ?? []).map((m) => ({ value: m.id, label: m.label }));
  }, [providerGroups, selectedProvider]);

  const canLoadModels = vk.trim().length > 0 && !loadingModels;
  const canSend = !sending && vk.trim() && selectedModel && inputText.trim();

  // Active-count badge on the Params button — quick sanity ("am I
  // sending temperature right now?") without opening the popover.
  const activeParamCount = useMemo(() => {
    let n = 0;
    for (const k of STANDARD_PARAM_ORDER) {
      if (stdParams[k].enabled) n++;
    }
    for (const c of customParams) {
      if (c.enabled && c.key.trim() !== '') n++;
    }
    return n;
  }, [stdParams, customParams]);

  // Auto-scroll the timeline to the bottom on new messages / streaming
  // deltas so the latest exchange is always in view (chat-app convention).
  const timelineRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (timelineRef.current) {
      timelineRef.current.scrollTop = timelineRef.current.scrollHeight;
    }
  }, [messages, streamingAssistant]);

  const setStdParam = (key: StandardParamKey, patch: Partial<ParamRowState>) => {
    setStdParams((prev) => ({ ...prev, [key]: { ...prev[key], ...patch } }));
  };
  const updateCustomParam = (id: string, patch: Partial<CustomParam>) => {
    setCustomParams((prev) => prev.map((p) => (p.id === id ? { ...p, ...patch } : p)));
  };
  const addCustomParam = () => {
    setCustomParams((prev) => [...prev, { id: newCustomId(), enabled: true, key: '', value: '' }]);
  };
  const removeCustomParam = (id: string) => {
    setCustomParams((prev) => prev.filter((p) => p.id !== id));
  };
  const resetParams = () => {
    setStdParams(makeInitialParams());
    setCustomParams([]);
  };

  const loadModels = async () => {
    setLoadError(null);
    setRequestError(null);
    setLoadingModels(true);
    try {
      const result = await aiGatewayClientSimulatorApi.listModels(gatewayBaseUrl, vk);
      const groups = groupModelsByProvider(result.data ?? []);
      setProviderGroups(groups);
      const firstProvider = groups[0]?.providerKey ?? '';
      setSelectedProvider(firstProvider);
      setSelectedModel(groups[0]?.models[0]?.id ?? '');
    } catch (err) {
      setProviderGroups([]);
      setSelectedProvider('');
      setSelectedModel('');
      setLoadError(err instanceof Error ? err.message : t('pages:aiGatewaySimulator.errors.loadModels'));
    } finally {
      setLoadingModels(false);
    }
  };

  const refreshUsage = async () => {
    try {
      const usage = await aiGatewayClientSimulatorApi.getUsage(gatewayBaseUrl, vk);
      setUsageSummary(usage);
    } catch (err) {
      setRequestError(err instanceof Error ? err.message : t('pages:aiGatewaySimulator.errors.usage'));
    }
  };

  const stopStreaming = () => {
    streamController?.abort();
    setStreamController(null);
    setSending(false);
  };

  const clearConversation = () => {
    setMessages([]);
    setStreamingAssistant('');
    setLastUsage(null);
    setRequestError(null);
  };

  const send = async () => {
    if (!canSend) return;
    setRequestError(null);
    const userMessage: SimulatorChatMessage = { role: 'user', content: inputText.trim() };

    const params = buildRequestParams(stdParams, customParams);
    const validationError = validateRequest(format, params);
    if (validationError) {
      setRequestError(validationError);
      return;
    }

    setSending(true);
    setMessages((prev) => [...prev, userMessage]);
    setInputText('');

    const sendArgs = {
      baseUrl: gatewayBaseUrl,
      vk,
      format,
      model: selectedModel,
      messages: [userMessage],
      params,
    };

    try {
      if (!stream) {
        const completion = await aiGatewayClientSimulatorApi.createChatCompletion(sendArgs);
        const content = completion.choices?.[0]?.message?.content ?? '';
        setMessages((prev) => [...prev, { role: 'assistant', content }]);
        setLastUsage(completion.usage ?? null);
        await refreshUsage();
      } else {
        const controller = new AbortController();
        setStreamController(controller);
        setStreamingAssistant('');
        let merged = '';
        await aiGatewayClientSimulatorApi.createChatCompletionStream(
          sendArgs,
          {
            onDelta: (delta) => {
              merged += delta;
              setStreamingAssistant(merged);
            },
            onDone: () => {
              if (merged) {
                setMessages((prev) => [...prev, { role: 'assistant', content: merged }]);
              }
              setStreamingAssistant('');
            },
            onUsage: (usage) => setLastUsage(usage),
          },
          controller.signal,
        );
        setStreamController(null);
        await refreshUsage();
      }
    } catch (err) {
      if (err instanceof Error && err.name === 'AbortError') return;
      setRequestError(err instanceof Error ? err.message : t('pages:aiGatewaySimulator.errors.send'));
    } finally {
      setSending(false);
    }
  };

  const onComposerKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      void send();
    }
  };

  return (
    <div className={styles.page}>
      <PageHeader
        title={t('pages:aiGatewaySimulator.title')}
        subtitle={t('pages:aiGatewaySimulator.subtitle')}
      />

      <div className={styles.toolbar}>
        <Stack direction="horizontal" gap="sm" align="center" style={{ flex: 1, minWidth: 0 }}>
          <div className={styles.toolbarSelect}>
            <Select
              value={format}
              onValueChange={(v) => setFormat(v as RequestFormat)}
              options={FORMAT_OPTIONS}
              placeholder={t('pages:aiGatewaySimulator.format', 'Format')}
            />
          </div>
          <div className={styles.toolbarSelect}>
            <Select
              value={selectedProvider}
              onValueChange={(provider) => {
                setSelectedProvider(provider);
                const firstModel = providerGroups.find((g) => g.providerKey === provider)?.models[0]?.id ?? '';
                setSelectedModel(firstModel);
              }}
              options={providerOptions}
              disabled={providerOptions.length === 0}
              placeholder={t('pages:aiGatewaySimulator.selectProvider')}
            />
          </div>
          <div className={styles.toolbarSelect}>
            <Select
              value={selectedModel}
              onValueChange={setSelectedModel}
              options={modelOptions}
              disabled={!selectedProvider}
              placeholder={t('pages:aiGatewaySimulator.selectModel')}
            />
          </div>
        </Stack>
        <Stack direction="horizontal" gap="sm" align="center">
          <Popover open={paramsOpen} onOpenChange={setParamsOpen}>
            <PopoverTrigger asChild>
              <Button variant="secondary">
                {t('pages:aiGatewaySimulator.params', 'Params')} ({activeParamCount})
              </Button>
            </PopoverTrigger>
            <PopoverContent align="end" sideOffset={6}>
              <div className={styles.paramsPanel}>
                <p className={styles.paramsSectionTitle}>
                  {t('pages:aiGatewaySimulator.standardParams', 'Standard parameters')}
                </p>
                {STANDARD_PARAM_ORDER.map((key) => {
                  const meta = STANDARD_PARAM_META[key];
                  const row = stdParams[key];
                  return (
                    <div key={key} className={styles.paramRow}>
                      <Checkbox
                        checked={row.enabled}
                        onCheckedChange={(c) => setStdParam(key, { enabled: Boolean(c) })}
                        aria-label={key}
                      />
                      <span className={styles.paramLabel}>{key}</span>
                      <Input
                        value={row.value}
                        onChange={(e) => setStdParam(key, { value: e.target.value })}
                        placeholder={meta.defaultValue || ''}
                        type={meta.kind === 'text' ? 'text' : 'text'}
                        disabled={!row.enabled}
                      />
                    </div>
                  );
                })}
                <p className={styles.paramsSectionTitle}>
                  {t('pages:aiGatewaySimulator.customParams', 'Custom parameters')}
                </p>
                <p className={styles.paramsHint}>
                  {t(
                    'pages:aiGatewaySimulator.customParamsHint',
                    'Values parse as JSON when possible (objects, numbers, booleans). Plain text passes through as a string. Custom keys override standard ones.',
                  )}
                </p>
                {customParams.map((row) => (
                  <div key={row.id} className={styles.customParamRow}>
                    <Checkbox
                      checked={row.enabled}
                      onCheckedChange={(c) => updateCustomParam(row.id, { enabled: Boolean(c) })}
                      aria-label={`custom ${row.key}`}
                    />
                    <Input
                      value={row.key}
                      onChange={(e) => updateCustomParam(row.id, { key: e.target.value })}
                      placeholder={t('pages:aiGatewaySimulator.customKey', 'key')}
                    />
                    <Input
                      value={row.value}
                      onChange={(e) => updateCustomParam(row.id, { value: e.target.value })}
                      placeholder={t('pages:aiGatewaySimulator.customValue', 'value or JSON')}
                    />
                    <button
                      type="button"
                      className={styles.deleteCustomParam}
                      onClick={() => removeCustomParam(row.id)}
                      aria-label={t('pages:aiGatewaySimulator.removeCustomParam', 'Remove')}
                    >
                      ×
                    </button>
                  </div>
                ))}
                <Button variant="secondary" onClick={addCustomParam}>
                  + {t('pages:aiGatewaySimulator.addCustomParam', 'Add custom parameter')}
                </Button>
                <div className={styles.paramsFooter}>
                  <Button variant="secondary" onClick={resetParams}>
                    {t('pages:aiGatewaySimulator.resetParams', 'Reset')}
                  </Button>
                  <Button onClick={() => setParamsOpen(false)}>
                    {t('pages:aiGatewaySimulator.doneParams', 'Done')}
                  </Button>
                </div>
              </div>
            </PopoverContent>
          </Popover>
          <Button variant="secondary" onClick={clearConversation} disabled={messages.length === 0 && !streamingAssistant}>
            {t('pages:aiGatewaySimulator.clear', 'Clear')}
          </Button>
          <Button variant="secondary" onClick={() => setSettingsOpen(true)}>
            {t('pages:aiGatewaySimulator.openSettings', 'Settings')}
          </Button>
        </Stack>
      </div>

      {loadError ? <ErrorBanner message={loadError} onRetry={loadModels} /> : null}
      {requestError ? <ErrorBanner message={requestError} /> : null}

      <div ref={timelineRef} className={styles.chatTimeline}>
        {messages.length === 0 && !streamingAssistant ? (
          <div className={styles.emptyState}>
            <p className={styles.emptyTitle}>{t('pages:aiGatewaySimulator.emptyTitle', 'Start a chat')}</p>
            <p className={styles.emptyHint}>
              {selectedModel
                ? t('pages:aiGatewaySimulator.emptyHintReady', 'Type a prompt below and press Cmd/Ctrl+Enter to send.')
                : t('pages:aiGatewaySimulator.emptyHintNoModel', 'Open Settings to set the gateway URL + virtual key, then Load models.')}
            </p>
          </div>
        ) : null}
        {messages.map((m, idx) => (
          <div
            key={`${m.role}-${idx}`}
            className={`${styles.bubbleRow} ${m.role === 'user' ? styles.bubbleRowUser : styles.bubbleRowAssistant}`}
          >
            <div className={`${styles.bubble} ${m.role === 'user' ? styles.bubbleUser : styles.bubbleAssistant}`}>
              <div className={styles.bubbleRole}>{m.role}</div>
              <div className={styles.bubbleContent}>{m.content}</div>
            </div>
          </div>
        ))}
        {streamingAssistant ? (
          <div className={`${styles.bubbleRow} ${styles.bubbleRowAssistant}`}>
            <div className={`${styles.bubble} ${styles.bubbleAssistant}`}>
              <div className={styles.bubbleRole}>assistant</div>
              <div className={styles.bubbleContent}>{streamingAssistant}</div>
            </div>
          </div>
        ) : null}
      </div>

      <div className={styles.composer}>
        <Textarea
          value={inputText}
          onChange={(e) => setInputText(e.target.value)}
          onKeyDown={onComposerKeyDown}
          placeholder={t('pages:aiGatewaySimulator.inputPlaceholder')}
          rows={3}
          className={styles.composerTextarea}
          aria-label={t('pages:aiGatewaySimulator.chatInputAria', 'chat input')}
        />
        <div className={styles.composerActions}>
          <Stack direction="horizontal" gap="sm" align="center">
            <Switch
              checked={stream}
              onCheckedChange={setStream}
              aria-label={t('pages:aiGatewaySimulator.streamMode')}
            />
            <span className={styles.streamLabel}>{t('pages:aiGatewaySimulator.streamMode')}</span>
          </Stack>
          <Stack direction="horizontal" gap="sm" align="center">
            {stream && sending ? (
              <Button variant="danger" onClick={stopStreaming}>
                {t('pages:aiGatewaySimulator.stop')}
              </Button>
            ) : null}
            <Button onClick={send} loading={sending} disabled={!canSend}>
              {t('pages:aiGatewaySimulator.send')}
            </Button>
          </Stack>
        </div>
      </div>

      <div className={styles.usageStrip}>
        <div className={styles.usageBlock}>
          <span className={styles.usageLabel}>{t('pages:aiGatewaySimulator.lastCompletionUsage')}</span>
          <span className={styles.usageValue}>
            {lastUsage
              ? `${lastUsage.prompt_tokens ?? '-'} / ${lastUsage.completion_tokens ?? '-'} / ${lastUsage.total_tokens ?? '-'}`
              : '—'}
          </span>
        </div>
        <div className={styles.usageBlock}>
          <span className={styles.usageLabel}>{t('pages:aiGatewaySimulator.summaryUsage')}</span>
          <span className={styles.usageValue}>
            {usageSummary?.usage
              ? `${usageSummary.usage.promptTokens ?? '-'} / ${usageSummary.usage.completionTokens ?? '-'} / ${usageSummary.usage.totalTokens ?? '-'} (${usageSummary.usage.totalRequests ?? '-'} req, $${usageSummary.usage.estimatedCostUsd ?? '-'})`
              : '—'}
          </span>
        </div>
        <Button variant="secondary" onClick={refreshUsage} disabled={!vk.trim()}>
          {t('pages:aiGatewaySimulator.refreshUsage')}
        </Button>
      </div>

      <Dialog
        open={settingsOpen}
        onOpenChange={setSettingsOpen}
        title={t('pages:aiGatewaySimulator.settingsTitle', 'Connection settings')}
      >
        <Stack gap="md">
          <FormField label={t('pages:aiGatewaySimulator.vk')}>
            <Input
              id="ai-gateway-simulator-vk"
              type="password"
              value={vk}
              onChange={(e) => setVk(e.target.value)}
              placeholder="nvk_xxx"
            />
          </FormField>

          <Stack direction="horizontal" gap="sm" justify="between" align="center">
            <Button onClick={loadModels} loading={loadingModels} disabled={!canLoadModels}>
              {t('pages:aiGatewaySimulator.loadModels')}
            </Button>
            <Button variant="secondary" onClick={() => setSettingsOpen(false)}>
              {t('pages:aiGatewaySimulator.closeSettings', 'Done')}
            </Button>
          </Stack>
        </Stack>
      </Dialog>
    </div>
  );
}
