import { describe, it, expect, vi, afterEach } from 'vitest';
import {
  aiGatewayClientSimulatorApi,
  buildOpenAIBody,
  buildAnthropicBody,
  buildGeminiBody,
  buildOpenAIResponsesBody,
  pathForRequest,
  validateRequest,
} from './aiGatewayClientSimulator';
import { api } from '../../client';

afterEach(() => {
  vi.restoreAllMocks();
});

describe('aiGatewayClientSimulatorApi', () => {
  it('forwards /v1/models through the CP api wrapper with target URL + VK in the body', async () => {
    const apiPostSpy = vi.spyOn(api, 'post').mockResolvedValue({ data: [] });

    await aiGatewayClientSimulatorApi.listModels('http://localhost:3050/', 'nvk_test');

    expect(apiPostSpy).toHaveBeenCalledWith('/api/admin/ai-gateway-simulator/forward', {
      targetUrl: 'http://localhost:3050',
      path: '/v1/models',
      method: 'GET',
      vk: 'nvk_test',
      body: undefined,
    });
  });

  it('OpenAI non-stream omits unchecked params and stays at /v1/chat/completions', async () => {
    const apiPostSpy = vi.spyOn(api, 'post').mockResolvedValue({ choices: [] });

    await aiGatewayClientSimulatorApi.createChatCompletion({
      baseUrl: 'http://localhost:3050',
      vk: 'nvk_test',
      format: 'openai',
      model: 'gpt-4o',
      messages: [{ role: 'user', content: 'hi' }],
      params: {},
    });

    expect(apiPostSpy).toHaveBeenCalledWith('/api/admin/ai-gateway-simulator/forward', {
      targetUrl: 'http://localhost:3050',
      path: '/v1/chat/completions',
      method: 'POST',
      vk: 'nvk_test',
      body: {
        model: 'gpt-4o',
        messages: [{ role: 'user', content: 'hi' }],
        stream: false,
      },
    });
  });

  it('OpenAI body includes only enabled params (Claude-style temperature-omission)', () => {
    const body = buildOpenAIBody({
      model: 'claude-opus-4-7',
      messages: [{ role: 'user', content: 'hi' }],
      params: { max_tokens: 1024 }, // temperature deliberately not set
      stream: false,
    });
    expect(body).toEqual({
      model: 'claude-opus-4-7',
      messages: [{ role: 'user', content: 'hi' }],
      stream: false,
      max_tokens: 1024,
    });
    expect(body).not.toHaveProperty('temperature');
  });

  it('OpenAI body prepends system prompt as a system-role message', () => {
    const body = buildOpenAIBody({
      model: 'gpt-4o',
      messages: [{ role: 'user', content: 'hi' }],
      params: { system: 'be terse' },
      stream: false,
    });
    expect(body.messages).toEqual([
      { role: 'system', content: 'be terse' },
      { role: 'user', content: 'hi' },
    ]);
  });

  it('Anthropic body uses top-level system + max_tokens (NOT a system-role message)', () => {
    const body = buildAnthropicBody({
      model: 'claude-sonnet-4-6',
      messages: [{ role: 'user', content: 'hi' }],
      params: { max_tokens: 256, temperature: 0.5, system: 'be terse' },
      stream: false,
    });
    expect(body).toEqual({
      model: 'claude-sonnet-4-6',
      messages: [{ role: 'user', content: 'hi' }],
      stream: false,
      max_tokens: 256,
      temperature: 0.5,
      system: 'be terse',
    });
  });

  it('Gemini body translates messages to contents array and groups tunables under generationConfig', () => {
    const body = buildGeminiBody({
      model: 'gemini-pro',
      messages: [
        { role: 'user', content: 'hi' },
        { role: 'assistant', content: 'hello' },
        { role: 'user', content: 'how are you' },
      ],
      params: { temperature: 0.3, max_tokens: 64, top_p: 0.9, system: 'be terse' },
      stream: false,
    });
    expect(body).toEqual({
      contents: [
        { role: 'user', parts: [{ text: 'hi' }] },
        { role: 'model', parts: [{ text: 'hello' }] },
        { role: 'user', parts: [{ text: 'how are you' }] },
      ],
      systemInstruction: { parts: [{ text: 'be terse' }] },
      generationConfig: { temperature: 0.3, maxOutputTokens: 64, topP: 0.9 },
    });
    expect(body).not.toHaveProperty('model');
  });

  it('pathForRequest returns the right Gemini path per stream flag', () => {
    expect(pathForRequest('openai', 'gpt-4o', false)).toBe('/v1/chat/completions');
    expect(pathForRequest('openai', 'gpt-4o', true)).toBe('/v1/chat/completions');
    expect(pathForRequest('anthropic', 'claude', false)).toBe('/v1/messages');
    expect(pathForRequest('gemini', 'gemini-pro', false)).toBe(
      '/v1beta/models/gemini-pro:generateContent',
    );
    expect(pathForRequest('gemini', 'gemini-pro', true)).toBe(
      '/v1beta/models/gemini-pro:streamGenerateContent',
    );
  });

  it('validateRequest gates Anthropic on max_tokens being set', () => {
    expect(validateRequest('anthropic', {})).toMatch(/max_tokens/i);
    expect(validateRequest('anthropic', { max_tokens: 0 })).toMatch(/max_tokens/i);
    expect(validateRequest('anthropic', { max_tokens: 100 })).toBeNull();
    expect(validateRequest('openai', {})).toBeNull();
    expect(validateRequest('gemini', {})).toBeNull();
  });

  it('customParams merge into the wire body and override standard fields', async () => {
    const apiPostSpy = vi.spyOn(api, 'post').mockResolvedValue({ choices: [] });

    await aiGatewayClientSimulatorApi.createChatCompletion({
      baseUrl: 'http://localhost:3050',
      vk: 'nvk_test',
      format: 'anthropic',
      model: 'claude-opus-4-7',
      messages: [{ role: 'user', content: 'hi' }],
      params: {
        max_tokens: 1024,
        customParams: {
          thinking: { type: 'enabled', budget_tokens: 2000 },
          // Deliberately overrides the standard max_tokens to verify
          // custom-wins precedence.
          max_tokens: 4096,
        },
      },
    });

    const call = apiPostSpy.mock.calls[0]?.[1] as { body: Record<string, unknown> };
    expect(call.body).toMatchObject({
      model: 'claude-opus-4-7',
      messages: [{ role: 'user', content: 'hi' }],
      stream: false,
      thinking: { type: 'enabled', budget_tokens: 2000 },
      max_tokens: 4096,
    });
  });

  it('parses OpenAI SSE stream and emits delta + done', async () => {
    const encoder = new TextEncoder();
    const streamBody = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(
          encoder.encode(
            'data: {"choices":[{"delta":{"content":"hello"}}]}\n\n' +
              'data: {"choices":[{"delta":{"content":" world"}}]}\n\n' +
              'data: [DONE]\n\n',
          ),
        );
        controller.close();
      },
    });
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(streamBody, { status: 200, headers: { 'Content-Type': 'text/event-stream' } }),
    );

    const onDelta = vi.fn();
    const onDone = vi.fn();
    await aiGatewayClientSimulatorApi.createChatCompletionStream(
      {
        baseUrl: 'http://localhost:3050',
        vk: 'nvk_test',
        format: 'openai',
        model: 'gpt-4o',
        messages: [{ role: 'user', content: 'hello' }],
        params: {},
      },
      { onDelta, onDone },
    );

    expect(onDelta).toHaveBeenCalledWith('hello');
    expect(onDelta).toHaveBeenCalledWith(' world');
    expect(onDone).toHaveBeenCalledTimes(1);
  });

  it('parses Anthropic SSE (content_block_delta text_delta) and emits done', async () => {
    const encoder = new TextEncoder();
    const streamBody = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(
          encoder.encode(
            'data: {"type":"message_start","message":{"usage":{"input_tokens":11,"output_tokens":1}}}\n\n' +
              'data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}\n\n' +
              'data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" there"}}\n\n' +
              'data: {"type":"message_delta","usage":{"input_tokens":11,"output_tokens":3}}\n\n' +
              'data: [DONE]\n\n',
          ),
        );
        controller.close();
      },
    });
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(streamBody, { status: 200, headers: { 'Content-Type': 'text/event-stream' } }),
    );

    const onDelta = vi.fn();
    const onDone = vi.fn();
    const onUsage = vi.fn();
    await aiGatewayClientSimulatorApi.createChatCompletionStream(
      {
        baseUrl: 'http://localhost:3050',
        vk: 'nvk_test',
        format: 'anthropic',
        model: 'claude-sonnet-4-6',
        messages: [{ role: 'user', content: 'hello' }],
        params: { max_tokens: 1024 },
      },
      { onDelta, onDone, onUsage },
    );

    expect(onDelta).toHaveBeenCalledWith('Hi');
    expect(onDelta).toHaveBeenCalledWith(' there');
    expect(onUsage).toHaveBeenCalledWith(
      expect.objectContaining({ prompt_tokens: 11, completion_tokens: 1, total_tokens: 12 }),
    );
    expect(onUsage).toHaveBeenCalledWith(
      expect.objectContaining({ prompt_tokens: 11, completion_tokens: 3, total_tokens: 14 }),
    );
    expect(onDone).toHaveBeenCalledTimes(1);
  });

  it('renders OpenAI-shape responses even when format=anthropic (gateway hub-normalizes)', async () => {
    // Real-world case: simulator sends to /v1/messages with format='anthropic',
    // but ai-gateway returns the response in canonical OpenAI shape (choices[]).
    // The page-side normalizer must still surface the assistant content rather
    // than render an empty bubble.
    vi.spyOn(api, 'post').mockResolvedValue({
      object: 'chat.completion',
      model: 'claude-opus-4-7',
      choices: [
        {
          index: 0,
          message: { role: 'assistant', content: 'Hello! How can I help you today?' },
          finish_reason: 'stop',
        },
      ],
      usage: { prompt_tokens: 13, completion_tokens: 15, total_tokens: 28 },
    });

    const res = await aiGatewayClientSimulatorApi.createChatCompletion({
      baseUrl: '',
      vk: 'nvk_test',
      format: 'anthropic',
      model: 'claude-opus-4-7',
      messages: [{ role: 'user', content: 'hello' }],
      params: { max_tokens: 1024 },
    });

    expect(res.choices?.[0]?.message?.content).toBe('Hello! How can I help you today?');
    expect(res.usage).toEqual({ prompt_tokens: 13, completion_tokens: 15, total_tokens: 28 });
  });

  it('renders Anthropic native shape when format=anthropic and gateway returns blocks', async () => {
    vi.spyOn(api, 'post').mockResolvedValue({
      id: 'msg_abc',
      type: 'message',
      role: 'assistant',
      content: [{ type: 'text', text: 'native anthropic reply' }],
      usage: { input_tokens: 7, output_tokens: 4 },
    });

    const res = await aiGatewayClientSimulatorApi.createChatCompletion({
      baseUrl: '',
      vk: 'nvk_test',
      format: 'anthropic',
      model: 'claude-opus-4-7',
      messages: [{ role: 'user', content: 'hi' }],
      params: { max_tokens: 1024 },
    });

    expect(res.choices?.[0]?.message?.content).toBe('native anthropic reply');
    expect(res.usage).toEqual({ prompt_tokens: 7, completion_tokens: 4, total_tokens: 11 });
  });

  it('parses Gemini SSE (candidates[].content.parts[].text) and emits done on EOF', async () => {
    const encoder = new TextEncoder();
    const streamBody = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(
          encoder.encode(
            'data: {"candidates":[{"content":{"parts":[{"text":"hi "}]}}]}\n\n' +
              'data: {"candidates":[{"content":{"parts":[{"text":"there"}]}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}\n\n',
          ),
        );
        controller.close();
      },
    });
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(streamBody, { status: 200, headers: { 'Content-Type': 'text/event-stream' } }),
    );

    const onDelta = vi.fn();
    const onDone = vi.fn();
    const onUsage = vi.fn();
    await aiGatewayClientSimulatorApi.createChatCompletionStream(
      {
        baseUrl: 'http://localhost:3050',
        vk: 'nvk_test',
        format: 'gemini',
        model: 'gemini-pro',
        messages: [{ role: 'user', content: 'hello' }],
        params: {},
      },
      { onDelta, onDone, onUsage },
    );

    expect(onDelta).toHaveBeenCalledWith('hi ');
    expect(onDelta).toHaveBeenCalledWith('there');
    expect(onUsage).toHaveBeenCalledWith(
      expect.objectContaining({ prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 }),
    );
    expect(onDone).toHaveBeenCalledTimes(1);
  });

  // OpenAI Responses-API simulator coverage

  it('pathForRequest returns /v1/responses for openai-responses format', () => {
    expect(pathForRequest('openai-responses', 'gpt-5.2', false)).toBe('/v1/responses');
    expect(pathForRequest('openai-responses', 'gpt-5.2', true)).toBe('/v1/responses');
  });

  it('openai-responses body uses input string shorthand for single user message', () => {
    const body = buildOpenAIResponsesBody({
      model: 'gpt-5.2',
      messages: [{ role: 'user', content: 'hello' }],
      params: {},
      stream: false,
    });
    expect(body.model).toBe('gpt-5.2');
    expect(body.input).toBe('hello');
    expect(body.stream).toBe(false);
    expect(body.instructions).toBeUndefined();
    expect(body.messages).toBeUndefined();
  });

  it('openai-responses body fans out multi-turn into input items + maps system → instructions', () => {
    const body = buildOpenAIResponsesBody({
      model: 'gpt-5.2',
      messages: [
        { role: 'user', content: 'Q1' },
        { role: 'assistant', content: 'A1' },
        { role: 'user', content: 'Q2' },
      ],
      params: { system: 'Be terse.', max_tokens: 200, temperature: 0.5 },
      stream: true,
    });
    expect(body.instructions).toBe('Be terse.');
    expect(body.max_output_tokens).toBe(200);
    expect(body.temperature).toBe(0.5);
    expect(body.stream).toBe(true);
    expect(Array.isArray(body.input)).toBe(true);
    const input = body.input as Array<{ role: string; content: Array<{ type: string; text: string }> }>;
    expect(input).toHaveLength(3);
    expect(input[0]).toEqual({ role: 'user', content: [{ type: 'input_text', text: 'Q1' }] });
    expect(input[1]).toEqual({ role: 'assistant', content: [{ type: 'input_text', text: 'A1' }] });
  });

  it('openai-responses body drops unsupported toolbar params (presence_penalty, seed, …)', () => {
    const body = buildOpenAIResponsesBody({
      model: 'gpt-5.2',
      messages: [{ role: 'user', content: 'hi' }],
      params: {
        max_tokens: 100,
        temperature: 0.3,
        top_p: 0.9,
        // Toolbar params that Responses-API doesn't support — must not leak.
        presence_penalty: 0.2,
        frequency_penalty: 0.1,
        seed: 42,
        stop: 'END',
      },
      stream: false,
    });
    expect(body.presence_penalty).toBeUndefined();
    expect(body.frequency_penalty).toBeUndefined();
    expect(body.seed).toBeUndefined();
    expect(body.stop).toBeUndefined();
    expect(body.max_output_tokens).toBe(100);
  });

  it('parses Responses-API SSE (response.output_text.delta + response.completed) and surfaces reasoning', async () => {
    const encoder = new TextEncoder();
    const streamBody = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(
          encoder.encode(
            'event: response.created\ndata: {"type":"response.created","response":{"id":"resp_x","status":"in_progress","model":"gpt-5.2"}}\n\n' +
              'event: response.reasoning_summary_text.delta\ndata: {"type":"response.reasoning_summary_text.delta","delta":"thinking..."}\n\n' +
              'event: response.output_text.delta\ndata: {"type":"response.output_text.delta","delta":"Hi"}\n\n' +
              'event: response.output_text.delta\ndata: {"type":"response.output_text.delta","delta":" there"}\n\n' +
              'event: response.completed\ndata: {"type":"response.completed","response":{"id":"resp_x","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}\n\n',
          ),
        );
        controller.close();
      },
    });
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(streamBody, { status: 200, headers: { 'Content-Type': 'text/event-stream' } }),
    );

    const onDelta = vi.fn();
    const onDone = vi.fn();
    const onUsage = vi.fn();
    await aiGatewayClientSimulatorApi.createChatCompletionStream(
      {
        baseUrl: 'http://localhost:3050',
        vk: 'nvk_test',
        format: 'openai-responses',
        model: 'gpt-5.2',
        messages: [{ role: 'user', content: 'hello' }],
        params: {},
      },
      { onDelta, onDone, onUsage },
    );

    expect(onDelta).toHaveBeenCalledWith('[reasoning] thinking...');
    expect(onDelta).toHaveBeenCalledWith('Hi');
    expect(onDelta).toHaveBeenCalledWith(' there');
    expect(onUsage).toHaveBeenCalledWith(
      expect.objectContaining({ prompt_tokens: 5, completion_tokens: 2, total_tokens: 7 }),
    );
    expect(onDone).toHaveBeenCalledTimes(1);
  });
});
