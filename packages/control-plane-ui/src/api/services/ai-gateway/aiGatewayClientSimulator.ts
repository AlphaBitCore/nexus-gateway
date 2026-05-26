import { api } from '../../client';
import { getAccessToken } from '../../../auth/tokens/tokenStore';

export interface SimulatorModelObject {
  id: string;
  /** Human-readable label from the gateway; falls back to `id` in the UI when absent. */
  name?: string;
  object?: string;
  created?: number;
  owned_by?: string;
  owner_display_name?: string;
}

export interface SimulatorModelListResponse {
  object?: string;
  data: SimulatorModelObject[];
}

export interface SimulatorChatMessage {
  role: 'system' | 'user' | 'assistant';
  content: string;
}

/** Wire-format the simulator UI lets the operator pick from. Each value
 * controls the endpoint path AND the request body shape — e.g. picking
 * "anthropic" sends to /v1/messages with Anthropic's required-max_tokens
 * top-level shape rather than chat.completions.
 *
 * "openai-responses" targets POST /v1/responses with the OpenAI
 * Responses-API shape (`input` instead of `messages`, `instructions` for
 * system-style guidance, `max_output_tokens` rename, `reasoning.effort`,
 * etc.) and parses the response.* SSE event stream.
 */
export type RequestFormat = 'openai' | 'anthropic' | 'gemini' | 'openai-responses';

/** Optional per-request knobs. A field that is `undefined` is omitted
 * from the wire body — the whole point of the toolbar's per-param
 * checkboxes is to avoid sending parameters a model rejects (e.g.
 * Claude Opus 4.7 rejecting `temperature`). */
export interface RequestParams {
  temperature?: number;
  max_tokens?: number;
  top_p?: number;
  presence_penalty?: number;
  frequency_penalty?: number;
  seed?: number;
  stop?: string;
  /** System prompt prepended per-format: as a `system` message
   * (OpenAI), top-level `system` field (Anthropic), or
   * `systemInstruction` (Gemini). */
  system?: string;
  /** Operator-supplied extra fields, merged into the wire body at the
   * root after the standard params. Used to reach provider-specific
   * knobs the simulator UI doesn't model (e.g. Anthropic
   * `thinking: {type:"enabled", budget_tokens:2000}`, Gemini
   * `safetySettings`, OpenAI `response_format`). Values that override
   * a standard param win. Each value should already be JSON-parsed by
   * the caller — strings stay strings, objects/arrays stay structured. */
  customParams?: Record<string, unknown>;
}

export interface SimulatorCompletionUsage {
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
}

/** Normalize usage from chat bodies or SSE (snake_case OpenAI or camelCase gateway). */
function normalizeSimulatorCompletionUsage(raw: unknown): SimulatorCompletionUsage | undefined {
  if (!raw || typeof raw !== 'object') return undefined;
  const o = raw as Record<string, unknown>;
  const pt = o.prompt_tokens ?? o.promptTokens;
  const ct = o.completion_tokens ?? o.completionTokens;
  const tt = o.total_tokens ?? o.totalTokens;
  const prompt_tokens = typeof pt === 'number' ? pt : undefined;
  const completion_tokens = typeof ct === 'number' ? ct : undefined;
  const total_tokens =
    typeof tt === 'number'
      ? tt
      : prompt_tokens !== undefined && completion_tokens !== undefined
        ? prompt_tokens + completion_tokens
        : undefined;
  if (
    prompt_tokens === undefined &&
    completion_tokens === undefined &&
    total_tokens === undefined
  ) {
    return undefined;
  }
  return { prompt_tokens, completion_tokens, total_tokens };
}

export interface SimulatorChatResponse {
  id?: string;
  choices?: Array<{
    index?: number;
    message?: SimulatorChatMessage;
    finish_reason?: string | null;
  }>;
  usage?: SimulatorCompletionUsage;
}

export interface SimulatorUsageSummaryResponse {
  virtualKeyId?: string;
  period?: string;
  periodType?: string;
  usage?: {
    totalRequests?: number;
    promptTokens?: number;
    completionTokens?: number;
    totalTokens?: number;
    estimatedCostUsd?: number;
  };
}

export interface SimulatorStreamCallbacks {
  onDelta: (delta: string) => void;
  onDone: () => void;
  onUsage?: (usage: SimulatorCompletionUsage) => void;
}

/** Text delta from OpenAI-style `chat.completion.chunk` frames. */
function extractOpenAIChatDelta(parsed: unknown): string | undefined {
  if (!parsed || typeof parsed !== 'object') return undefined;
  const p = parsed as { choices?: Array<{ delta?: { content?: string | null } }> };
  const c = p.choices?.[0]?.delta?.content;
  return typeof c === 'string' && c.length > 0 ? c : undefined;
}

/** Text delta from Anthropic Messages SSE `content_block_delta` + `text_delta` frames. */
function extractAnthropicTextDelta(parsed: unknown): string | undefined {
  if (!parsed || typeof parsed !== 'object') return undefined;
  const p = parsed as { type?: string; delta?: { type?: string; text?: string } };
  if (p.type !== 'content_block_delta' || !p.delta || typeof p.delta !== 'object') return undefined;
  if (p.delta.type !== 'text_delta') return undefined;
  const t = p.delta.text;
  return typeof t === 'string' && t.length > 0 ? t : undefined;
}

/** Text delta from Gemini `streamGenerateContent` SSE frames. Each
 * frame is a full GenerateContentResponse with one or more candidates
 * whose content.parts may carry incremental text. */
function extractGeminiTextDelta(parsed: unknown): string | undefined {
  if (!parsed || typeof parsed !== 'object') return undefined;
  const p = parsed as {
    candidates?: Array<{ content?: { parts?: Array<{ text?: string }> } }>;
  };
  const parts = p.candidates?.[0]?.content?.parts;
  if (!Array.isArray(parts)) return undefined;
  const text = parts
    .map((part) => (typeof part?.text === 'string' ? part.text : ''))
    .join('');
  return text.length > 0 ? text : undefined;
}

/** Text delta from OpenAI Responses-API SSE `response.output_text.delta` frames. */
function extractResponsesAPITextDelta(parsed: unknown): string | undefined {
  if (!parsed || typeof parsed !== 'object') return undefined;
  const p = parsed as { type?: string; delta?: string };
  if (p.type !== 'response.output_text.delta') return undefined;
  return typeof p.delta === 'string' && p.delta.length > 0 ? p.delta : undefined;
}

/** Reasoning summary delta from Responses-API SSE `response.reasoning_summary_text.delta`
 * (or the newer `response.reasoning_text.delta`) frames. Surfaced inline in
 * the simulator transcript with an `[reasoning] ` prefix so the operator
 * sees the model's chain-of-thought separately from the answer. */
function extractResponsesAPIReasoningDelta(parsed: unknown): string | undefined {
  if (!parsed || typeof parsed !== 'object') return undefined;
  const p = parsed as { type?: string; delta?: string };
  if (
    p.type !== 'response.reasoning_summary_text.delta' &&
    p.type !== 'response.reasoning_text.delta'
  ) {
    return undefined;
  }
  return typeof p.delta === 'string' && p.delta.length > 0 ? `[reasoning] ${p.delta}` : undefined;
}

/** Usage from Responses-API SSE `response.completed` event. Payload shape:
 *   { type:"response.completed", response:{ ..., usage:{ input_tokens, output_tokens,
 *     total_tokens, input_tokens_details:{cached_tokens},
 *     output_tokens_details:{reasoning_tokens} } } }
 */
function responsesAPIUsageToSimulator(u: {
  input_tokens?: unknown;
  output_tokens?: unknown;
  total_tokens?: unknown;
}): SimulatorCompletionUsage | undefined {
  const prompt = typeof u.input_tokens === 'number' ? u.input_tokens : undefined;
  const completion = typeof u.output_tokens === 'number' ? u.output_tokens : undefined;
  const total = typeof u.total_tokens === 'number' ? u.total_tokens : undefined;
  if (prompt === undefined && completion === undefined && total === undefined) return undefined;
  return {
    prompt_tokens: prompt,
    completion_tokens: completion,
    total_tokens:
      total ??
      (prompt !== undefined && completion !== undefined ? prompt + completion : undefined),
  };
}

function anthropicUsageToSimulator(u: {
  input_tokens?: unknown;
  output_tokens?: unknown;
}): SimulatorCompletionUsage | undefined {
  const prompt = typeof u.input_tokens === 'number' ? u.input_tokens : undefined;
  const completion = typeof u.output_tokens === 'number' ? u.output_tokens : undefined;
  if (prompt === undefined && completion === undefined) return undefined;
  const total =
    prompt !== undefined && completion !== undefined ? prompt + completion : undefined;
  return {
    prompt_tokens: prompt,
    completion_tokens: completion,
    total_tokens: total,
  };
}

/** Gemini usageMetadata → simulator's prompt/completion/total triple. */
function geminiUsageToSimulator(u: {
  promptTokenCount?: unknown;
  candidatesTokenCount?: unknown;
  totalTokenCount?: unknown;
}): SimulatorCompletionUsage | undefined {
  const prompt = typeof u.promptTokenCount === 'number' ? u.promptTokenCount : undefined;
  const completion = typeof u.candidatesTokenCount === 'number' ? u.candidatesTokenCount : undefined;
  const total = typeof u.totalTokenCount === 'number' ? u.totalTokenCount : undefined;
  if (prompt === undefined && completion === undefined && total === undefined) return undefined;
  return {
    prompt_tokens: prompt,
    completion_tokens: completion,
    total_tokens:
      total ??
      (prompt !== undefined && completion !== undefined ? prompt + completion : undefined),
  };
}

/** Usage from OpenAI chunk.usage / Anthropic message_start|message_delta /
 * Gemini usageMetadata / Responses-API response.completed payloads. */
function extractStreamUsage(parsed: unknown): SimulatorCompletionUsage | undefined {
  if (!parsed || typeof parsed !== 'object') return undefined;
  const p = parsed as Record<string, unknown>;

  // Responses-API response.completed carries usage under response.usage.
  // Check before the OpenAI top-level usage path because the
  // chat-completions stream encoder also emits top-level usage on its
  // finish chunk — we want the Responses-shape envelope detected first.
  if (p.type === 'response.completed' && p.response && typeof p.response === 'object') {
    const usage = (p.response as { usage?: unknown }).usage;
    if (usage && typeof usage === 'object') {
      return responsesAPIUsageToSimulator(
        usage as { input_tokens?: unknown; output_tokens?: unknown; total_tokens?: unknown },
      );
    }
  }

  const openaiUsage = normalizeSimulatorCompletionUsage(p.usage);
  if (openaiUsage) return openaiUsage;

  if (p.type === 'message_start' && p.message && typeof p.message === 'object') {
    const usage = (p.message as { usage?: unknown }).usage;
    if (usage && typeof usage === 'object') {
      return anthropicUsageToSimulator(usage as { input_tokens?: unknown; output_tokens?: unknown });
    }
  }

  if (p.type === 'message_delta' && p.usage && typeof p.usage === 'object') {
    return anthropicUsageToSimulator(p.usage as { input_tokens?: unknown; output_tokens?: unknown });
  }

  if (p.usageMetadata && typeof p.usageMetadata === 'object') {
    return geminiUsageToSimulator(
      p.usageMetadata as {
        promptTokenCount?: unknown;
        candidatesTokenCount?: unknown;
        totalTokenCount?: unknown;
      },
    );
  }

  return undefined;
}

function normalizeBaseUrl(baseUrl: string): string {
  const trimmed = baseUrl.trim();
  return trimmed.endsWith('/') ? trimmed.slice(0, -1) : trimmed;
}

async function parseError(res: Response): Promise<Error> {
  const fallback = `Request failed (${res.status})`;
  try {
    const json = (await res.json()) as { error?: { message?: string } | string };
    if (typeof json?.error === 'string') return new Error(json.error);
    if (json?.error && typeof json.error === 'object' && typeof json.error.message === 'string') {
      return new Error(json.error.message);
    }
  } catch {
    // Ignore body parse errors and use fallback text.
  }
  return new Error(fallback);
}

// --- Per-format request building ----------------------------------------

/** Strip `undefined`-valued keys so they never reach the wire body. */
function pruneUndefined<T extends Record<string, unknown>>(obj: T): Partial<T> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(obj)) {
    if (v !== undefined) out[k] = v;
  }
  return out as Partial<T>;
}

interface BuildArgs {
  model: string;
  messages: SimulatorChatMessage[];
  params: RequestParams;
  stream: boolean;
}

/** OpenAI chat.completions wire body. `system` is prepended as a
 * system-role message because OpenAI doesn't take a top-level system
 * field. `stop` (UI string) is wrapped to a single-element array. */
export function buildOpenAIBody({ model, messages, params, stream }: BuildArgs): Record<string, unknown> {
  const finalMessages =
    typeof params.system === 'string' && params.system.length > 0
      ? [{ role: 'system' as const, content: params.system }, ...messages]
      : messages;
  return pruneUndefined({
    model,
    messages: finalMessages,
    stream,
    temperature: params.temperature,
    max_tokens: params.max_tokens,
    top_p: params.top_p,
    presence_penalty: params.presence_penalty,
    frequency_penalty: params.frequency_penalty,
    seed: params.seed,
    stop: typeof params.stop === 'string' && params.stop.length > 0 ? [params.stop] : undefined,
  });
}

/** Anthropic Messages wire body. Anthropic requires `max_tokens` at the
 * top level — callers must validate this before invoking the build (the
 * simulator UI surfaces it as a hard gate on Send). `system` is a
 * top-level field, NOT a message. */
export function buildAnthropicBody({ model, messages, params, stream }: BuildArgs): Record<string, unknown> {
  return pruneUndefined({
    model,
    messages: messages.map((m) => ({ role: m.role, content: m.content })),
    stream,
    max_tokens: params.max_tokens,
    temperature: params.temperature,
    top_p: params.top_p,
    system: typeof params.system === 'string' && params.system.length > 0 ? params.system : undefined,
    stop_sequences:
      typeof params.stop === 'string' && params.stop.length > 0 ? [params.stop] : undefined,
  });
}

/** Gemini generateContent wire body. messages → contents array, role
 * `assistant` becomes `model`. system → top-level systemInstruction.
 * Tunables go under `generationConfig`. The model name does NOT appear
 * in the body — Gemini puts it in the URL path. */
export function buildGeminiBody({ messages, params, stream }: BuildArgs): Record<string, unknown> {
  const generationConfig = pruneUndefined({
    temperature: params.temperature,
    maxOutputTokens: params.max_tokens,
    topP: params.top_p,
    seed: params.seed,
    stopSequences:
      typeof params.stop === 'string' && params.stop.length > 0 ? [params.stop] : undefined,
  });
  return pruneUndefined({
    contents: messages.map((m) => ({
      role: m.role === 'assistant' ? 'model' : m.role,
      parts: [{ text: m.content }],
    })),
    systemInstruction:
      typeof params.system === 'string' && params.system.length > 0
        ? { parts: [{ text: params.system }] }
        : undefined,
    generationConfig: Object.keys(generationConfig).length > 0 ? generationConfig : undefined,
    // Gemini doesn't carry `stream` in the body — the stream-vs-not
    // distinction is encoded in the path (:streamGenerateContent vs
    // :generateContent). Field intentionally omitted; `stream` arg
    // referenced here so eslint no-unused-vars stays happy.
    ...(stream ? {} : {}),
  });
}

/** OpenAI Responses-API wire body. `input` is either a string shorthand
 * (single user message) or an array of input items (multi-turn).
 * `instructions` carries system-level guidance (maps from the simulator's
 * `system` param). `max_tokens` maps to `max_output_tokens`.
 * `presence_penalty` / `frequency_penalty` / `seed` are not accepted by
 * this surface and are dropped silently. */
export function buildOpenAIResponsesBody({ model, messages, params, stream }: BuildArgs): Record<string, unknown> {
  // Convert messages array into Responses input items. A single user
  // message uses the string-shorthand form; everything else fans out
  // into the full input-item array so previous turns + system prompt
  // round-trip cleanly.
  let input: unknown;
  if (messages.length === 1 && messages[0]?.role === 'user' && typeof messages[0].content === 'string') {
    input = messages[0].content;
  } else {
    input = messages.map((m) => ({
      role: m.role,
      content: [{ type: 'input_text', text: m.content }],
    }));
  }
  return pruneUndefined({
    model,
    input,
    instructions:
      typeof params.system === 'string' && params.system.length > 0 ? params.system : undefined,
    stream,
    temperature: params.temperature,
    top_p: params.top_p,
    max_output_tokens: params.max_tokens,
  });
}

/** Path the simulator should target for a given (format, model, stream)
 * triple. */
export function pathForRequest(format: RequestFormat, _model: string, stream: boolean): string {
  switch (format) {
    case 'openai':
      return '/v1/chat/completions';
    case 'anthropic':
      return '/v1/messages';
    case 'gemini':
      return `/v1beta/models/${encodeURIComponent(_model)}:${stream ? 'streamGenerateContent' : 'generateContent'}`;
    case 'openai-responses':
      return '/v1/responses';
  }
}

function buildBody(format: RequestFormat, args: BuildArgs): Record<string, unknown> {
  let base: Record<string, unknown>;
  switch (format) {
    case 'openai':
      base = buildOpenAIBody(args);
      break;
    case 'anthropic':
      base = buildAnthropicBody(args);
      break;
    case 'gemini':
      base = buildGeminiBody(args);
      break;
    case 'openai-responses':
      base = buildOpenAIResponsesBody(args);
      break;
  }
  // Custom params merge in at the root, AFTER the standard build, so an
  // operator can override a standard field (e.g. force a different
  // model name shape) and reach provider-specific extensions the
  // simulator UI doesn't surface as first-class checkboxes.
  if (args.params.customParams) {
    for (const [k, v] of Object.entries(args.params.customParams)) {
      if (v !== undefined) base[k] = v;
    }
  }
  return base;
}

/** Per-format pre-flight validation. Returns null on OK, a
 * human-readable error string otherwise. Anthropic Messages requires
 * max_tokens at the top level — sending without it gets a 400 from the
 * API and an unhelpful "validation_error" in the simulator transcript;
 * surfacing the hint here keeps the operator from chasing a non-bug. */
export function validateRequest(format: RequestFormat, params: RequestParams): string | null {
  if (format === 'anthropic' && (params.max_tokens === undefined || params.max_tokens <= 0)) {
    return 'Anthropic Messages requires max_tokens — enable it in Params.';
  }
  return null;
}

// FORWARD_ENDPOINT routes the simulator's calls through the Control
// Plane backend so the browser only ever talks to the same HTTPS origin
// as the rest of the admin UI. Hits a same-origin URL → no
// mixed-content blocker even when the target ai-gateway is HTTP.
//
// The CP handler validates the admin session (Bearer access token from
// the project tokenStore — same auth scheme every other admin API uses),
// enforces an SSRF allowlist on the target path + scheme, attaches the
// gateway VK as the upstream Bearer auth, and streams the response back
// chunk-by-chunk so SSE responses keep their real-time delivery.
const FORWARD_ENDPOINT = '/api/admin/ai-gateway-simulator/forward';

interface ForwardRequest {
  targetUrl: string;
  path: string;
  method: 'GET' | 'POST';
  vk: string;
  body?: unknown;
}

function buildForwardPayload(req: ForwardRequest) {
  return {
    targetUrl: normalizeBaseUrl(req.targetUrl),
    path: req.path,
    method: req.method,
    vk: req.vk.trim(),
    body: req.body,
  };
}

// rawForwardFetch is the streaming-friendly path: project's `api.post`
// wrapper returns parsed JSON, but the SSE stream needs the raw Response
// reader. Replicates the wrapper's auth: read the access token from the
// shared tokenStore and stamp Authorization: Bearer. No refresh-on-401
// retry on this path — a long stream with an expired token simply
// surfaces the 401, which is acceptable for an admin debugging tool.
async function rawForwardFetch(req: ForwardRequest, signal?: AbortSignal): Promise<Response> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  const token = getAccessToken();
  if (token) headers['Authorization'] = `Bearer ${token}`;
  return fetch(FORWARD_ENDPOINT, {
    method: 'POST',
    headers,
    body: JSON.stringify(buildForwardPayload(req)),
    signal,
  });
}

export interface SendChatArgs {
  baseUrl: string;
  vk: string;
  format: RequestFormat;
  model: string;
  messages: SimulatorChatMessage[];
  params: RequestParams;
}

/** Non-stream chat completion. The shape of the returned envelope is
 * normalized to OpenAI's `choices[0].message.content` regardless of
 * upstream format so the timeline rendering stays uniform. */
async function createChatCompletion(args: SendChatArgs): Promise<SimulatorChatResponse> {
  const path = pathForRequest(args.format, args.model, false);
  const body = buildBody(args.format, {
    model: args.model,
    messages: args.messages,
    params: args.params,
    stream: false,
  });
  const json = await api.post<unknown>(
    FORWARD_ENDPOINT,
    buildForwardPayload({
      targetUrl: args.baseUrl,
      path,
      method: 'POST',
      vk: args.vk,
      body,
    }),
  );
  return normalizeNonStreamResponse(args.format, json);
}

/** Per-format response → OpenAI-shape `SimulatorChatResponse` adapter
 * so the timeline doesn't need to know which format it's rendering.
 *
 * Defensive shape detection: OpenAI canonical shape (`choices[].message`)
 * is tried FIRST regardless of declared format because the gateway
 * frequently normalizes responses to canonical OpenAI even on
 * non-OpenAI ingress paths (the canonicalbridge hub design). Falling
 * back to format-specific shapes only when the canonical shape is
 * absent keeps the timeline working whether or not a given path
 * round-trips through the response-side bridge. */
function normalizeNonStreamResponse(format: RequestFormat, raw: unknown): SimulatorChatResponse {
  if (!raw || typeof raw !== 'object') return {};
  const r = raw as Record<string, unknown>;

  // Try OpenAI canonical shape first — it covers OpenAI ingress AND any
  // cross-format request the gateway has hub-normalized.
  const choices = r.choices as
    | Array<{ index?: number; message?: { role?: string; content?: string }; finish_reason?: string | null }>
    | undefined;
  if (
    Array.isArray(choices) &&
    choices.length > 0 &&
    typeof choices[0]?.message?.content === 'string'
  ) {
    const out = r as SimulatorChatResponse;
    const u = normalizeSimulatorCompletionUsage(out.usage);
    if (u) out.usage = u;
    return out;
  }

  // Anthropic native shape: `content` is an array of typed blocks.
  if (Array.isArray(r.content)) {
    const blocks = r.content as Array<{ type?: string; text?: string }>;
    const text = blocks
      .filter((c) => c?.type === 'text' && typeof c.text === 'string')
      .map((c) => c.text as string)
      .join('');
    if (text.length > 0) {
      const u = anthropicUsageToSimulator(
        (r.usage as { input_tokens?: unknown; output_tokens?: unknown } | undefined) ?? {},
      );
      return {
        id: typeof r.id === 'string' ? r.id : undefined,
        choices: [{ index: 0, message: { role: 'assistant', content: text } }],
        usage: u,
      };
    }
  }

  // OpenAI Responses-API native shape: output[] is an array of typed
  // items — `message` carries content[].text; `reasoning` carries
  // summary[].text; others pass through untyped. Reasoning is prepended
  // so the operator sees chain-of-thought before the answer.
  if (typeof r.object === 'string' && r.object === 'response' && Array.isArray(r.output)) {
    const output = r.output as Array<{
      type?: string;
      content?: Array<{ type?: string; text?: string }>;
      summary?: Array<{ type?: string; text?: string }>;
    }>;
    const reasoningParts: string[] = [];
    const textParts: string[] = [];
    for (const item of output) {
      if (item?.type === 'reasoning' && Array.isArray(item.summary)) {
        for (const s of item.summary) {
          if (s?.type === 'summary_text' && typeof s.text === 'string') {
            reasoningParts.push(s.text);
          }
        }
      } else if (item?.type === 'message' && Array.isArray(item.content)) {
        for (const c of item.content) {
          if (c?.type === 'output_text' && typeof c.text === 'string') {
            textParts.push(c.text);
          }
        }
      }
    }
    const content = [
      reasoningParts.length > 0 ? `[reasoning] ${reasoningParts.join('')}\n\n` : '',
      textParts.join(''),
    ].join('');
    if (content.length > 0) {
      const u = responsesAPIUsageToSimulator(
        (r.usage as { input_tokens?: unknown; output_tokens?: unknown; total_tokens?: unknown } | undefined) ?? {},
      );
      return {
        id: typeof r.id === 'string' ? r.id : undefined,
        choices: [{ index: 0, message: { role: 'assistant', content } }],
        usage: u,
      };
    }
  }

  // Gemini native shape: candidates[].content.parts[].text.
  if (Array.isArray(r.candidates)) {
    const candidates = r.candidates as Array<{
      content?: { parts?: Array<{ text?: string }> };
    }>;
    const text = (candidates[0]?.content?.parts ?? [])
      .map((p) => (typeof p?.text === 'string' ? p.text : ''))
      .join('');
    if (text.length > 0) {
      const u = geminiUsageToSimulator(
        (r.usageMetadata as {
          promptTokenCount?: unknown;
          candidatesTokenCount?: unknown;
          totalTokenCount?: unknown;
        } | undefined) ?? {},
      );
      return {
        choices: [{ index: 0, message: { role: 'assistant', content: text } }],
        usage: u,
      };
    }
  }

  // Last-resort: caller picked a format but the body matched none of the
  // recognised shapes. Surface an empty assistant message so the UI
  // shows something rather than silently dropping the response — the
  // operator can inspect the network panel to see the raw payload.
  // `format` is referenced in the stub message so a future shape
  // mismatch is at least labelled with what was expected.
  return {
    choices: [
      {
        index: 0,
        message: {
          role: 'assistant',
          content: `(no recognised content for format=${format})`,
        },
      },
    ],
  };
}

/** Streaming chat completion. The simulator timeline only needs text
 * deltas + a final usage block, so we collapse OpenAI / Anthropic /
 * Gemini SSE shapes into a single delta-and-usage callback contract. */
async function createChatCompletionStream(
  args: SendChatArgs,
  callbacks: SimulatorStreamCallbacks,
  signal?: AbortSignal,
): Promise<void> {
  const path = pathForRequest(args.format, args.model, true);
  const body = buildBody(args.format, {
    model: args.model,
    messages: args.messages,
    params: args.params,
    stream: true,
  });
  const res = await rawForwardFetch(
    {
      targetUrl: args.baseUrl,
      path,
      method: 'POST',
      vk: args.vk,
      body,
    },
    signal,
  );
  if (!res.ok) throw await parseError(res);
  if (!res.body) throw new Error('SSE stream body is empty');

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  let doneNotified = false;

  const handleEventPayload = (payload: string) => {
    const data = payload.trim();
    if (!data) return;
    if (data === '[DONE]') {
      doneNotified = true;
      callbacks.onDone();
      return;
    }
    try {
      const parsed: unknown = JSON.parse(data);
      const delta =
        extractOpenAIChatDelta(parsed) ??
        extractAnthropicTextDelta(parsed) ??
        extractGeminiTextDelta(parsed) ??
        extractResponsesAPITextDelta(parsed) ??
        extractResponsesAPIReasoningDelta(parsed);
      if (delta) callbacks.onDelta(delta);
      const usage = extractStreamUsage(parsed);
      if (usage && callbacks.onUsage) {
        const u = normalizeSimulatorCompletionUsage(usage) ?? usage;
        callbacks.onUsage(u);
      }
    } catch {
      // Ignore non-JSON frames to keep stream resilient.
    }
  };

  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    const events = buffer.split('\n\n');
    buffer = events.pop() ?? '';
    for (const evt of events) {
      const lines = evt.split('\n');
      const chunks: string[] = [];
      for (const line of lines) {
        if (line.startsWith('data:')) chunks.push(line.slice(5).trimStart());
      }
      if (chunks.length > 0) {
        handleEventPayload(chunks.join('\n'));
      }
    }
  }

  if (!doneNotified) {
    const tail = buffer.trim();
    if (tail.includes('data: [DONE]')) {
      callbacks.onDone();
    } else if (!doneNotified) {
      // Anthropic + Gemini terminate the stream with an EOF instead of
      // a `data: [DONE]` sentinel; treat a clean reader EOF as
      // completion rather than an error.
      callbacks.onDone();
    }
  }
}

export const aiGatewayClientSimulatorApi = {
  async listModels(baseUrl: string, vk: string): Promise<SimulatorModelListResponse> {
    return api.post<SimulatorModelListResponse>(
      FORWARD_ENDPOINT,
      buildForwardPayload({ targetUrl: baseUrl, path: '/v1/models', method: 'GET', vk }),
    );
  },

  createChatCompletion,
  createChatCompletionStream,

  async getUsage(baseUrl: string, vk: string): Promise<SimulatorUsageSummaryResponse> {
    return api.post<SimulatorUsageSummaryResponse>(
      FORWARD_ENDPOINT,
      buildForwardPayload({ targetUrl: baseUrl, path: '/v1/usage', method: 'GET', vk }),
    );
  },
};
