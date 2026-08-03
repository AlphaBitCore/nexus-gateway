import { useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  aiGatewayClientSimulatorApi,
  validateRequest,
  type RequestFormat,
  type SimulatorChatMessage,
  type SimulatorCompletionUsage,
  type SimulatorUsageSummaryResponse,
} from '@/api/services/ai-gateway/aiGatewayClientSimulator';
import {
  type CustomParam,
  type ParamRowState,
  type ProviderGroup,
  type StandardParamKey,
  buildRequestParams,
  groupModelsByProvider,
} from './simulatorParams';

interface SendInput {
  stdParams: Record<StandardParamKey, ParamRowState>;
  customParams: CustomParam[];
}

export interface UseChatState {
  vk: string;
  setVk: (vk: string) => void;
  providerGroups: ProviderGroup[];
  selectedProvider: string;
  setSelectedProvider: (provider: string) => void;
  selectedModel: string;
  setSelectedModel: (model: string) => void;
  stream: boolean;
  setStream: (stream: boolean) => void;
  format: RequestFormat;
  setFormat: (format: RequestFormat) => void;
  inputText: string;
  setInputText: (text: string) => void;
  messages: SimulatorChatMessage[];
  lastUsage: SimulatorCompletionUsage | null;
  usageSummary: SimulatorUsageSummaryResponse | null;
  streamingAssistant: string;
  loadError: string | null;
  loadingModels: boolean;
  requestError: string | null;
  sending: boolean;
  providerOptions: Array<{ value: string; label: string }>;
  modelOptions: Array<{ value: string; label: string }>;
  canLoadModels: boolean;
  canSend: boolean | string;
  timelineRef: React.RefObject<HTMLDivElement | null>;
  loadModels: () => Promise<void>;
  refreshUsage: () => Promise<void>;
  stopStreaming: () => void;
  clearConversation: () => void;
  send: (input: SendInput) => Promise<void>;
}

export function useChatState(): UseChatState {
  const { t } = useTranslation();
  // Connection — gateway URL is resolved server-side (CP resolves the
  // ai-gateway's Hub-reported address at request time; no local config,
  // no fallback). UI only needs the operator's VK.
  const gatewayBaseUrl = '';
  const [vk, setVk] = useState('');
  const [providerGroups, setProviderGroups] = useState<ProviderGroup[]>([]);
  const [selectedProvider, setSelectedProvider] = useState('');
  const [selectedModel, setSelectedModel] = useState('');
  const [stream, setStream] = useState(false);
  const [format, setFormat] = useState<RequestFormat>('openai');
  // Chat state.
  const [inputText, setInputText] = useState('');
  const [messages, setMessages] = useState<SimulatorChatMessage[]>([]);
  const [lastUsage, setLastUsage] = useState<SimulatorCompletionUsage | null>(null);
  const [usageSummary, setUsageSummary] = useState<SimulatorUsageSummaryResponse | null>(null);
  const [streamingAssistant, setStreamingAssistant] = useState('');
  const [streamController, setStreamController] = useState<AbortController | null>(null);
  // UX state.
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

  // Auto-scroll the timeline to the bottom on new messages / streaming
  // deltas so the latest exchange is always in view (chat-app convention).
  const timelineRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (timelineRef.current) {
      timelineRef.current.scrollTop = timelineRef.current.scrollHeight;
    }
  }, [messages, streamingAssistant]);

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

  const send = async ({ stdParams, customParams }: SendInput) => {
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

  return {
    vk,
    setVk,
    providerGroups,
    selectedProvider,
    setSelectedProvider,
    selectedModel,
    setSelectedModel,
    stream,
    setStream,
    format,
    setFormat,
    inputText,
    setInputText,
    messages,
    lastUsage,
    usageSummary,
    streamingAssistant,
    loadError,
    loadingModels,
    requestError,
    sending,
    providerOptions,
    modelOptions,
    canLoadModels,
    canSend,
    timelineRef,
    loadModels,
    refreshUsage,
    stopStreaming,
    clearConversation,
    send,
  };
}
