// Sampling-quirk decision registry — the machine-checkable half of
// provider-adapter-architecture.md §3a Rule 7 for the sampling-params
// incident class (temperature / top_p rejected by specific model families).
//
// Consumed by:
//   scripts/check-quirk-coverage.mjs — every chat model in
//     tools/db-migrate/model-catalog.json must match one family entry here;
//     every `strips` decision must anchor to real adapter code, and every
//     strip RULE (per goFile, across the rows citing it) must be exercisable
//     by at least one seeded model; the smoke's rejects-temperature set must
//     not go stale against the catalog.
//   scripts/check-quirk-evidence.mjs — every registered quirk-rule site in Go
//     carries an observed-400 citation; every `strips` decision here cites
//     its 400.
//
// HOW TO ADD A FAMILY (the lint sent you here):
//   1. Probe the vendor: send the new family a request WITH temperature.
//   2. Record the decision below, quoting the vendor's own response.
//   3. If the vendor rejects: add/extend the owning adapter's quirk rule
//      (§3a Rule 3 — the rule lives with the adapter that talks to that
//      wire), cite the observed 400 above it, and point goFile/goMatch at it.
// A speculative strip is forbidden; a missing decision fails CI.
//
// Decision values:
//   'strips'          — the GATEWAY strips the params (goFile/goMatch anchor
//                       the rule; the smoke proves the strip by SENDING the
//                       param — 200 required). Usually because the vendor
//                       rejects them; when the vendor actually accepts and
//                       the family rule over-strips, the evidence says so
//                       (see the gpt-5.4 row).
//   'accepts'         — probed: the vendor honours caller sampling params;
//                       the gateway forwards them. May carry a goFile/goMatch
//                       anchor when a codec allowlist encodes the fact — the
//                       lint verifies every anchored row, so removing the
//                       family from the allowlist re-reddens here.
//   'forward-unprobed'— no sampling-param 400 ever observed on this
//                       provider; params forward verbatim. Explicitly
//                       recorded so a new family is a conscious decision,
//                       not an accident. NOTE: these are catch-all rows —
//                       new families on these providers do NOT fail the
//                       lint; the recorded posture (§3a Rule 7: no
//                       speculative strips) is the decision.

const OPENAI_REWRITES = 'packages/ai-gateway/internal/providers/specs/openai/rewrites/rewrites.go';
const MOONSHOT_REWRITES = 'packages/ai-gateway/internal/providers/specs/compat/moonshot/rewrites.go';
const DEEPSEEK_REWRITES = 'packages/ai-gateway/internal/providers/specs/compat/deepseek/rewrites.go';
// The per-model sampling policy (allowlist + predicates) lives in sampling.go,
// split out of codec.go for the file-size ratchet; all anthropic sampling
// anchors below point here.
const ANTHROPIC_CODEC = 'packages/ai-gateway/internal/providers/specs/anthropic/codec/sampling.go';

export const families = [
  // ── openai ────────────────────────────────────────────────────────────────
  {
    provider: 'openai', prefix: 'gpt-5.6', decision: 'strips',
    goFile: OPENAI_REWRITES, goMatch: 'specutil.GenerationAtLeast(modelID, "gpt-", 5)',
    evidence: 'Probed 2026-07-16 (api.openai.com, gpt-5.6-luna): chat 400 "Unsupported value: \'temperature\' does not support 0 with this model. Only the default (1) value is supported."; /v1/responses 400 "Unsupported parameter: \'temperature\' is not supported with this model." First observed 2026-07 --all-ingress smoke on all three -luna/-sol/-terra.',
  },
  {
    provider: 'openai', prefix: 'gpt-5.5', decision: 'strips',
    goFile: OPENAI_REWRITES, goMatch: 'specutil.GenerationAtLeast(modelID, "gpt-", 5)',
    evidence: 'Probed 2026-07-16 (api.openai.com): chat 400 "Unsupported value: \'temperature\' does not support 0 with this model. Only the default (1) value is supported."',
  },
  {
    provider: 'openai', prefix: 'gpt-5.4', decision: 'accepts',
    goFile: OPENAI_REWRITES, goMatch: '"gpt-5.4"',
    evidence: 'Probed 2026-07-16 (api.openai.com, gpt-5.4 and gpt-5.4-mini): temperature=0 answers 200 on BOTH chat and /v1/responses — the family\'s only real quirk is the max_tokens rename (400 "Use \'max_completion_tokens\' instead", probed same day; NeedsMaxTokensRename keeps covering it). The anchor is the RejectsSamplingParams carve-out: removing it re-opens the over-strip.',
  },
  {
    provider: 'openai', prefix: 'o1', decision: 'strips',
    goFile: OPENAI_REWRITES, goMatch: 'RejectsSamplingParams',
    evidence: 'Probed 2026-07-16 (api.openai.com): o1 400 "Unsupported parameter: \'temperature\' is not supported with this model." Rule matches o+digit programmatically.',
  },
  {
    provider: 'openai', prefix: 'o3', decision: 'strips',
    goFile: OPENAI_REWRITES, goMatch: 'RejectsSamplingParams',
    evidence: 'Probed 2026-07-16 (api.openai.com): o3 400 "Unsupported value: \'temperature\' does not support 0 with this model. Only the default (1) value is supported." (temperature=1 answers 200); o3-mini 400 "Unsupported parameter: \'temperature\'".',
  },
  {
    provider: 'openai', prefix: 'o4', decision: 'strips',
    goFile: OPENAI_REWRITES, goMatch: 'RejectsSamplingParams',
    evidence: 'Probed 2026-07-16 (api.openai.com): o4-mini 400 "Unsupported value: \'temperature\' does not support 0 with this model. Only the default (1) value is supported."',
  },
  {
    provider: 'openai', prefix: 'gpt-audio', decision: 'accepts',
    evidence: 'Probed 2026-08-05 (api.openai.com, gpt-audio-1.5 and gpt-audio-mini): temperature=0 and temperature=1.0 both answer 200 on chat completions, with an input_audio part present. Probed WITHOUT audio first and got 400 "This model requires that either input content or output modality contain audio." — identical with and without temperature, so that 400 is the modality requirement and not a sampling rejection; recording a decision from it would have been a guess wearing a probe\'s clothes. These are chat-completions models that accept and emit audio parts, which is why they are typed chat rather than the retired "audio".',
  },
  {
    provider: 'openai', prefix: 'gpt-4o', decision: 'accepts',
    evidence: 'Probed 2026-07-16 (api.openai.com): gpt-4o temperature=0 answers 200. Smoke P3 sends temperature=0 on every run.',
  },
  {
    provider: 'openai', prefix: 'gpt-4-turbo', decision: 'accepts',
    evidence: 'Probed 2026-07-16 (api.openai.com): temperature=0 answers 200. Smoke P3 sends temperature=0 on every run.',
  },
  {
    provider: 'openai', prefix: 'gpt-4.1', decision: 'accepts',
    evidence: 'Probed 2026-07-16 (api.openai.com): gpt-4.1 temperature=0 answers 200. Smoke P3 sends temperature=0 on every run.',
  },

  // ── azure-openai (same wire contract; the azure adapter constructs its
  //    identity codec from the shared openai contract — specs/azure/spec.go) ─
  {
    provider: 'azure-openai', prefix: 'gpt-5', decision: 'strips',
    goFile: OPENAI_REWRITES, goMatch: 'specutil.GenerationAtLeast(modelID, "gpt-", 5)',
    evidence: 'Same wire contract as the openai rows: Azure serves the same model families and returns the same 400 for sampling params on reasoning models; the azure adapter constructs its identity codec from the shared openai contract (specs/azure/spec.go).',
  },
  {
    provider: 'azure-openai', prefix: 'gpt-5.4', decision: 'accepts',
    goFile: OPENAI_REWRITES, goMatch: '"gpt-5.4"',
    evidence: 'Same vendor models as the openai gpt-5.4 row (probed 2026-07-16: temperature 200 on both wires; only the max_tokens rename is real). Azure deployments are not seeded, so the probe evidence rides the openai row; the shared contract applies the same RejectsSamplingParams carve-out.',
  },
  {
    provider: 'azure-openai', prefix: 'o1', decision: 'strips',
    goFile: OPENAI_REWRITES, goMatch: 'RejectsSamplingParams',
    evidence: 'Same vendor models and 400 as the openai o1 row (probed 2026-07-16 on api.openai.com); the azure adapter constructs its identity codec from the shared openai contract.',
  },
  {
    provider: 'azure-openai', prefix: 'o3', decision: 'strips',
    goFile: OPENAI_REWRITES, goMatch: 'RejectsSamplingParams',
    evidence: 'Same vendor models and 400 as the openai o3 row (probed 2026-07-16 on api.openai.com); the azure adapter constructs its identity codec from the shared openai contract.',
  },
  {
    provider: 'azure-openai', prefix: 'o4', decision: 'strips',
    goFile: OPENAI_REWRITES, goMatch: 'RejectsSamplingParams',
    evidence: 'Same vendor models and 400 as the openai o4 row (probed 2026-07-16 on api.openai.com); the azure adapter constructs its identity codec from the shared openai contract.',
  },
  {
    provider: 'azure-openai', prefix: 'gpt-4o', decision: 'accepts',
    evidence: 'Same vendor models as the openai gpt-4o row (temperature honoured); Azure deployments are not seeded, so the probe evidence rides the openai rows.',
  },
  {
    provider: 'azure-openai', prefix: 'gpt-4.1', decision: 'accepts',
    evidence: 'Same vendor models as the openai gpt-4.1 row; not seeded, probe evidence rides the openai rows.',
  },

  // ── anthropic ─────────────────────────────────────────────────────────────
  // The codec is an ALLOWLIST (claudeModelsAcceptingSamplingParams): any
  // claude family NOT listed there is stripped, so new families fail safe.
  // The accepts rows below mirror the allowlist; their goMatch anchors are
  // verified by check-quirk-coverage (every anchored row is checked), so
  // removing a family from the codec allowlist without a probe re-reddens
  // here. The strip only fires on the cross-format leg today — the native
  // /v1/messages leg forwards verbatim, which is why every stripped claude
  // family must also sit in the smoke's _REASONING_MODELS set (lint check 6).
  {
    provider: 'anthropic', prefix: 'claude-opus-4-6', decision: 'accepts',
    goFile: ANTHROPIC_CODEC, goMatch: '"claude-opus-4-6"',
    evidence: 'Probed 2026-07-16 (api.anthropic.com): temperature=0 answers 200. The 4.x families reject the temperature+top_p combination (codec drops top_p when both present).',
  },
  {
    provider: 'anthropic', prefix: 'claude-opus-4-5', decision: 'accepts',
    goFile: ANTHROPIC_CODEC, goMatch: '"claude-opus-4-5"',
    evidence: 'Probed against api.anthropic.com (codec allowlist): accepts temperature or top_p alone; rejects the combination.',
  },
  {
    provider: 'anthropic', prefix: 'claude-sonnet-4-6', decision: 'accepts',
    goFile: ANTHROPIC_CODEC, goMatch: '"claude-sonnet-4-6"',
    evidence: 'Probed against api.anthropic.com (codec allowlist): accepts temperature or top_p alone; rejects the combination.',
  },
  {
    provider: 'anthropic', prefix: 'claude-sonnet-4-5', decision: 'accepts',
    goFile: ANTHROPIC_CODEC, goMatch: '"claude-sonnet-4-5"',
    evidence: 'Probed against api.anthropic.com (codec allowlist): accepts temperature or top_p alone; rejects the combination.',
  },
  {
    provider: 'anthropic', prefix: 'claude-haiku-4-5', decision: 'accepts',
    goFile: ANTHROPIC_CODEC, goMatch: '"claude-haiku-4-5"',
    evidence: 'Probed 2026-07-16 (api.anthropic.com): temperature=0 alone answers 200; temperature+top_p together 400 "`temperature` and `top_p` cannot both be specified for this model."',
  },
  {
    provider: 'anthropic', prefix: 'claude-', decision: 'strips',
    goFile: ANTHROPIC_CODEC, goMatch: 'claudeModelsAcceptingSamplingParams',
    evidence: 'Probed 2026-07-16 (api.anthropic.com): claude-opus-4-7, claude-opus-4-8, claude-sonnet-5, claude-fable-5 each answer 400 "`temperature` is deprecated for this model." Allowlist inversion: every claude family outside claudeModelsAcceptingSamplingParams is stripped, so new families fail safe.',
  },

  // ── google-gemini ─────────────────────────────────────────────────────────
  {
    provider: 'google-gemini', prefix: 'gemini-2.5', decision: 'accepts',
    evidence: 'Probed 2026-07-16 (generativelanguage.googleapis.com): gemini-2.5-flash generationConfig.temperature=0 answers 200. Smoke P3G sends it on every run.',
  },
  {
    provider: 'google-gemini', prefix: 'gemini-3.1', decision: 'accepts',
    evidence: 'Probed 2026-07-16 (generativelanguage.googleapis.com): gemini-3.1-flash-lite generationConfig.temperature=0 answers 200.',
  },
  {
    provider: 'google-gemini', prefix: 'gemini-3.5', decision: 'accepts',
    evidence: 'Probed 2026-07-16 (generativelanguage.googleapis.com): gemini-3.5-flash generationConfig.temperature=0 answers 200.',
  },
  {
    provider: 'google-gemini', prefix: 'gemini-3.6', decision: 'accepts',
    evidence: 'Probed 2026-08-06 (generativelanguage.googleapis.com): gemini-3.6-flash generationConfig.temperature=0 with maxOutputTokens=1 answers 200. Probed when the family was added to the catalog — this lint is what required it.',
  },

  // ── deepseek ──────────────────────────────────────────────────────────────
  {
    provider: 'deepseek', prefix: 'deepseek-v4', decision: 'accepts',
    evidence: 'Probed 2026-07-16 (api.deepseek.com): deepseek-v4-flash and deepseek-v4-pro temperature=0 answer 200. DeepSeek thinking quirks are tool_choice / reasoning_content back-fill (specs/compat/deepseek/rewrites.go), not sampling params.',
  },

  // ── moonshot ──────────────────────────────────────────────────────────────
  // The adapter is an ALLOWLIST (kimiFamiliesAcceptingTemperature): any `kimi-`
  // family NOT listed there is stripped, so new families fail safe. Same shape
  // and same reason as the anthropic block above. It was a denylist of the
  // three k2.x families until 2026-08-06; the k2.7 incident IS the denylist
  // going stale, and the vendor's /v1/models carried kimi-k3 that day while
  // this registry and the catalog carried neither.
  {
    provider: 'moonshot', prefix: 'kimi-', decision: 'strips',
    goFile: MOONSHOT_REWRITES, goMatch: 'kimiFamiliesAcceptingTemperature',
    evidence: 'Probed 2026-07-16 (api.moonshot.cn): kimi-k2.5 and kimi-k2.6 answer 400 "invalid temperature: only 1 is allowed for this model", 200 with the param omitted. kimi-k2.7-code and -highspeed: observed first on production traffic (the incident this registry exists for), re-probed the same day with the same 400. Allowlist inversion: every kimi family outside kimiFamiliesAcceptingTemperature is stripped, so an unprobed family costs a dropped parameter instead of a 400.',
  },
  {
    provider: 'moonshot', prefix: 'kimi-k2-thinking', decision: 'accepts',
    goFile: MOONSHOT_REWRITES, goMatch: '"kimi-k2-thinking"',
    evidence: 'Probed historically (api.moonshot.ai — recorded with the fixed-temp rule in specs/compat/moonshot/rewrites.go): accepts arbitrary temperature. Re-probe 2026-07-16 on api.moonshot.cn answered 404 "Not found the model kimi-k2-thinking or Permission denied" on the test key — vendor truth unverifiable there; the historical probe stands. Anchored on its allowlist entry, so dropping it from the codec without a probe re-reddens here.',
  },
  {
    provider: 'moonshot', prefix: 'moonshot-v1-', decision: 'accepts',
    evidence: 'Probed 2026-07-16 (api.moonshot.cn): moonshot-v1-8k temperature=0 answers 200; family accepts arbitrary temperature per the fixed-temp rule doc in specs/compat/moonshot/rewrites.go.',
  },

  // ── providers with no observed sampling-param quirk ──────────────────────
  // Params forward verbatim. §3a Rule 7 forbids a speculative strip: a rule
  // is born only from an observed 400 in traffic_event, with its citation.
  {
    provider: 'minimax', prefix: '', decision: 'forward-unprobed',
    evidence: 'No sampling-param 400 observed on this provider; forwarded verbatim until traffic_event shows one.',
  },
  {
    provider: 'glm', prefix: '', decision: 'forward-unprobed',
    evidence: 'No sampling-param 400 observed on this provider; forwarded verbatim until traffic_event shows one.',
  },
  {
    provider: 'xai', prefix: '', decision: 'forward-unprobed',
    evidence: 'No sampling-param 400 observed on this provider; forwarded verbatim until traffic_event shows one.',
  },
  {
    provider: 'groq', prefix: '', decision: 'forward-unprobed',
    evidence: 'No sampling-param 400 observed on this provider; forwarded verbatim until traffic_event shows one.',
  },
  {
    provider: 'together', prefix: '', decision: 'forward-unprobed',
    evidence: 'No sampling-param 400 observed on this provider; forwarded verbatim until traffic_event shows one. Hosted kimi/glm/deepseek builds speak Together\'s wire, not the vendor\'s — the moonshot/glm rows do not transfer.',
  },
  {
    provider: 'perplexity', prefix: '', decision: 'forward-unprobed',
    evidence: 'No sampling-param 400 observed on this provider; forwarded verbatim until traffic_event shows one.',
  },
  {
    provider: 'fireworks', prefix: '', decision: 'forward-unprobed',
    evidence: 'No sampling-param 400 observed on this provider; forwarded verbatim until traffic_event shows one. Hosted kimi/deepseek/glm builds speak Fireworks\' wire — vendor rows do not transfer.',
  },
  {
    provider: 'mistral', prefix: '', decision: 'forward-unprobed',
    evidence: 'No sampling-param 400 observed on this provider; forwarded verbatim until traffic_event shows one.',
  },
  {
    provider: 'cohere', prefix: '', decision: 'forward-unprobed',
    evidence: 'No sampling-param 400 observed on this provider; forwarded verbatim until traffic_event shows one.',
  },
  {
    provider: 'huggingface', prefix: '', decision: 'forward-unprobed',
    evidence: 'No sampling-param 400 observed on this provider; forwarded verbatim until traffic_event shows one.',
  },
  {
    provider: 'replicate', prefix: '', decision: 'forward-unprobed',
    evidence: 'No sampling-param 400 observed on this provider; forwarded verbatim until traffic_event shows one. Replicate-hosted claude/gpt speak Replicate\'s wire — the anthropic/openai rows do not transfer.',
  },
  {
    provider: 'bedrock', prefix: '', decision: 'forward-unprobed',
    evidence: 'No sampling-param 400 observed on this provider; forwarded verbatim until traffic_event shows one. Bedrock-hosted claude speaks the Bedrock wire via its own codec — the anthropic rows do not transfer.',
  },
  {
    provider: 'vertex', prefix: '', decision: 'forward-unprobed',
    evidence: 'No sampling-param 400 observed on this provider; forwarded verbatim until traffic_event shows one. Vertex-hosted gemini/claude speak the Vertex wire — the google-gemini/anthropic rows do not transfer.',
  },
];

// Every Go site that encodes a per-model wire-quirk rule (§3a Rule 7).
// check-quirk-evidence.mjs asserts the contiguous comment block above each
// symbol cites an observed vendor rejection (must mention "400" and
// observed/probed language). When a rule moves (e.g. into a codec), update
// the row in the same commit — the lint fails on a missing symbol.
export const goQuirkSites = [
  { file: OPENAI_REWRITES, symbol: 'RejectsSamplingParams', kind: 'func' },
  { file: OPENAI_REWRITES, symbol: 'NeedsMaxTokensRename', kind: 'func' },
  { file: OPENAI_REWRITES, symbol: 'IsAda002Embedding', kind: 'func' },
  { file: OPENAI_REWRITES, symbol: 'RequiresEffortNoneWithTools', kind: 'func' },
  { file: MOONSHOT_REWRITES, symbol: 'IsFixedTempModel', kind: 'func' },
  { file: DEEPSEEK_REWRITES, symbol: 'IsThinkingModel', kind: 'func' },
  { file: DEEPSEEK_REWRITES, symbol: 'fillMissingReasoningContent', kind: 'func' },
  { file: ANTHROPIC_CODEC, symbol: 'claudeModelsAcceptingSamplingParams', kind: 'var' },
  { file: ANTHROPIC_CODEC, symbol: 'anthropicModelRejectsTempTopPTogether', kind: 'func' },
];

export const CATALOG_PATH = 'tools/db-migrate/model-catalog.json';
export const SMOKE_PATH = 'tests/scripts/smoke-gateway.py';

// ── Reasoning-parameter decision registry ───────────────────────────────────
//
// A SECOND incident class, kept in this file because it obeys the same rules
// (§3a Rule 3: the quirk lives with the adapter that talks to that wire; Rule 7:
// no speculative behaviour, a missing decision fails CI) and asks the same
// question of a different parameter: does this model family take a REASONING
// request parameter, and in which shape?
//
// Scope is narrower than the sampling registry's on purpose. Only the models
// the catalog tags `reasoning` are in scope, because only they can be sent one.
//
// Decision values:
//   'translates'      — the adapter converts the caller's canonical intent into
//                       this wire's own shape (a level into a budget, or the
//                       reverse). goFile/goMatch anchor the translating code,
//                       and the lint verifies the anchor, so deleting the
//                       translation reddens here.
//   'forwards'        — probed: the wire takes the canonical spelling as it
//                       stands, so nothing is translated.
//   'forward-unprobed'— nobody has probed whether this wire takes a reasoning
//                       parameter. It forwards verbatim and NO cross-shape
//                       translation is invented for it. Recorded so a family is
//                       a conscious posture rather than an accident.
//
// Why so many rows are unprobed: probing costs money on a real vendor account,
// and the standing rule is that the gateway smoke runs only at the final
// matrix. A guessed budget range is an upstream 400, so the honest posture is
// to forward what the caller wrote and translate nothing.

const ANTHROPIC_REASONING = 'packages/ai-gateway/internal/providers/specs/anthropic/codec/codec.go';
const GEMINI_REASONING = 'packages/ai-gateway/internal/providers/specs/gemini/codec/codec.go';

export const reasoningFamilies = [
  // ── the three wire families that actually take a reasoning parameter ──────
  {
    provider: 'openai', prefix: '', decision: 'forwards',
    evidence: 'The canonical carries a LEVEL and this wire takes a LEVEL: `reasoning_effort` on chat, `reasoning.effort` on /v1/responses. Nothing is translated for a caller who wrote a level. The gpt-5.6 rule — a non-empty tools[] forces an explicit "none", and the ABSENT field 400s identically — is a SAMPLING-shaped field rule and lives in specs/openai/rewrites with its own observed-400 citation; it is not a reasoning translation.',
  },
  {
    provider: 'azure-openai', prefix: '', decision: 'forwards',
    evidence: 'Same wire contract as openai: the azure adapter constructs its identity codec from the shared openai contract (specs/azure/spec.go), so the reasoning parameter behaves identically.',
  },
  {
    provider: 'anthropic', prefix: 'claude-', decision: 'translates',
    goFile: ANTHROPIC_REASONING, goMatch: 'thinking',
    evidence: 'This wire takes a BUDGET (`thinking{type,budget_tokens}`), so a caller who wrote a level needs converting. The per-model rules are probe-derived and already in this codec: minimum budget 1024, requires max_tokens > budget_tokens, clamps to max_tokens-1, hard 400 when max_tokens <= 1024, skipped on type:"disabled".',
  },
  {
    provider: 'bedrock', prefix: '', decision: 'translates',
    goFile: ANTHROPIC_REASONING, goMatch: 'thinking',
    evidence: 'Delegates chat to the anthropic codec (specs/bedrock/codec.go), so it inherits the same budget contract and the same translation.',
  },
  {
    provider: 'google-gemini', prefix: '', decision: 'translates',
    goFile: GEMINI_REASONING, goMatch: 'thinkingConfig',
    evidence: 'This wire takes a BUDGET plus a FLAG (`generationConfig.thinkingConfig`), and carries a value neither other family has: -1 means dynamic. IncludeThoughts maps directly.',
  },
  {
    provider: 'vertex', prefix: '', decision: 'translates',
    goFile: GEMINI_REASONING, goMatch: 'thinkingConfig',
    evidence: 'Uses the gemini codec verbatim (specs/vertex/spec.go), so it inherits the same shape.',
  },

  // ── OpenAI-compatible wires: a reasoning-TAGGED model whose wire has no
  //    reasoning request knob recorded anywhere. The tag describes how the
  //    model behaves, not a parameter we may send it. ──────────────────────
  {
    provider: 'deepseek', prefix: '', decision: 'forward-unprobed',
    evidence: 'No reasoning request knob found in specs/compat/deepseek. Its IsThinkingModel rules repair a tool-call HISTORY (strip a forced tool_choice, back-fill reasoning_content) — a different parameter class. Whether this wire accepts reasoning_effort is unprobed.',
  },
  { provider: 'moonshot', prefix: '', decision: 'forward-unprobed',
    evidence: 'No reasoning request knob in specs/compat/moonshot; kimi-k2-thinking appears only in a temperature accepts-list. Unprobed.' },
  { provider: 'glm', prefix: '', decision: 'forward-unprobed', evidence: 'Identity codec, no reasoning request knob in specs/glm. Unprobed.' },
  { provider: 'minimax', prefix: '', decision: 'forward-unprobed', evidence: 'Identity codec, no reasoning request knob in specs/minimax. Unprobed.' },
  { provider: 'xai', prefix: '', decision: 'forward-unprobed', evidence: 'Identity codec, no reasoning request knob in specs/xai. Unprobed.' },
  { provider: 'groq', prefix: '', decision: 'forward-unprobed', evidence: 'Identity codec, no reasoning request knob in specs/groq. Unprobed.' },
  { provider: 'together', prefix: '', decision: 'forward-unprobed', evidence: 'Identity codec, no reasoning request knob in specs/together. Unprobed.' },
  { provider: 'perplexity', prefix: '', decision: 'forward-unprobed', evidence: 'Identity codec, no reasoning request knob in specs/perplexity. Unprobed.' },
  { provider: 'fireworks', prefix: '', decision: 'forward-unprobed', evidence: 'Identity codec, no reasoning request knob in specs/fireworks. Unprobed.' },
  { provider: 'mistral', prefix: '', decision: 'forward-unprobed', evidence: 'Identity codec, no reasoning request knob in specs/mistral. Unprobed.' },
  {
    provider: 'replicate', prefix: '', decision: 'forward-unprobed',
    evidence: 'An explicit input{} allowlist (specs/replicate/codec.go) DROPS the parameter rather than forwarding it. Recorded as unprobed because whether the underlying model would have taken one was never established — the drop is our allowlist, not the wire\'s answer.',
  },
];
