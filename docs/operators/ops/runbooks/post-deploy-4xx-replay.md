# Post-deploy 4xx replay

Upstream 4xx classes the gateway is built to absorb, and the request that proves
each one is absorbed on a given deployment. Run after a deploy, or whenever a
deployment starts showing one of these classes in Traffic.

## Check the running version first

Every class below is handled in gateway code. A deployment showing one is
usually running a binary older than the fix, not exposing a defect. Establish
the version before debugging the class — the answer is often that the node is
behind.

```bash
curl -s "$CP_URL/api/admin/nodes" -H "authorization: Bearer $TOKEN" \
  | python3 -c "import json,sys;[print(f\"{n['id']:34s} {n['type']:16s} {n.get('version')}\") for n in json.load(sys.stdin)['nodes']]"
```

A class that still reproduces on a current binary is a defect. A class that
disappears on redeploy was a stale binary.

## Replays

`$GW` is the gateway base URL, `$VK` a virtual key with access to the model.
Substitute a model of the same family that the deployment actually carries.

### Anthropic output ceiling

The Anthropic wire requires `max_tokens`, so the codec emits one from the
catalog's `maxOutputTokens` when the caller omits it. A catalog row above the
vendor's real ceiling makes the gateway's own fill fail:

```
max_tokens: 131072 > 128000, which is the maximum allowed number of output tokens for claude-opus-4-7
```

The same row is what `/v1/models` advertises, so a caller who echoes the
advertised ceiling back is rejected by a number the gateway published. Treat any
occurrence as a catalog-data defect, not a codec defect.

```bash
# Must answer 200. A 400 naming a ceiling below what /v1/models advertises for
# this model means the catalog row is above the vendor ceiling.
curl -s -o /dev/null -w '%{http_code}\n' "$GW/v1/chat/completions" \
  -H "authorization: Bearer $VK" -H 'content-type: application/json' \
  -d '{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}'
```

Fix without a deploy: **Providers → the provider → Models → Sync from Catalog** diffs
every provider row against the built-in catalog and offers the corrections. A
deployment that has never run it can carry wrong ceilings indefinitely.

### Gemini tool schemas

Gemini's `Schema` is proto-backed, so a JSON Schema key it does not know fails
the whole request:

```
Invalid JSON payload received. Unknown name "type" at 'tools[0].function_declarations[0]...': Proto field is not repeating, cannot start list.
Invalid JSON payload received. Unknown name "$comment" at '...': Cannot find field.
```

`specs/gemini/codec/schema_sanitize.go` holds an allow-list of the probed-accepted
keys plus semantic conversions (`type:["T","null"]` → `type` + `nullable`).

```bash
# A union type, a $comment and additionalProperties in one tool schema.
# Must answer 200.
curl -s -o /dev/null -w '%{http_code}\n' "$GW/v1/chat/completions" \
  -H "authorization: Bearer $VK" -H 'content-type: application/json' \
  -d '{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"call the tool"}],
       "tools":[{"type":"function","function":{"name":"t","parameters":{
         "type":"object","additionalProperties":false,"$comment":"note",
         "properties":{"a":{"type":["string","null"]}}}}}]}'
```

### DeepSeek thinking mode

A replayed tool loop whose assistant turns carry no `reasoning_content`:

```
The `reasoning_content` in the thinking mode must be passed back to the API.
```

`fillMissingReasoningContent` in `specs/compat/deepseek/rewrites.go` fills every
assistant message that has non-empty `tool_calls` and no `reasoning_content`
with `""`, which the upstream accepts as a presence marker. The gate matches the
whole `deepseek-v4` family and `deepseek-reasoner`.

```bash
# An assistant tool_calls turn with NO reasoning_content. Must answer 200.
curl -s -o /dev/null -w '%{http_code}\n' "$GW/v1/chat/completions" \
  -H "authorization: Bearer $VK" -H 'content-type: application/json' \
  -d '{"model":"deepseek-v4-pro","messages":[
        {"role":"user","content":"what time is it"},
        {"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function",
          "function":{"name":"clock","arguments":"{}"}}]},
        {"role":"tool","tool_call_id":"c1","content":"12:00"}],
       "tools":[{"type":"function","function":{"name":"clock","parameters":{"type":"object","properties":{}}}}]}'
```

### Sampling parameters a family rejects

Three vendors reject a caller-supplied `temperature` on some families —
`invalid temperature: only 1 is allowed for this model` (Moonshot),
`Unsupported parameter: 'temperature' is not supported with this model`
(OpenAI), `` `temperature` is deprecated for this model `` (Anthropic). The
gateway strips the parameter and names the strip on `X-Nexus-Coerced`.

Each vendor's rule is an accepts-list over its namespace, so a family nobody has
probed is stripped rather than forwarded: an unrecognised family costs a dropped
parameter, never a 400.

```bash
# Send temperature to a rejecting family. Must answer 200 and report the strip.
curl -s -D- -o /dev/null "$GW/v1/chat/completions" \
  -H "authorization: Bearer $VK" -H 'content-type: application/json' \
  -d '{"model":"kimi-k2.7-code","messages":[{"role":"user","content":"hi"}],"temperature":0.3}' \
  | grep -i 'HTTP/\|x-nexus-coerced'
```

Repeat against a `gpt-5.6-*` and a rejecting `claude-*`. All must be 200 with an
`X-Nexus-Coerced` naming the removed parameter.

### A vendor model the catalog still marks active

A model the vendor has withdrawn answers 404 with its own explanation, for
example `Claude Fable 5 is not available. Please use Opus 4.8.` There is no
codec fix: the catalog row's lifecycle is the fix. The catalog-sync dialog
cannot carry it — the provider template has no `status`, `deprecationDate` or
`replacedBy` — so retiring the row is a manual admin edit on the model.

```bash
curl -s "$GW/v1/chat/completions" -H "authorization: Bearer $VK" \
  -H 'content-type: application/json' \
  -d '{"model":"claude-fable-5","messages":[{"role":"user","content":"hi"}]}' | head -c 200
```

## Correct behaviour — do not "fix" these

These appear alongside the classes above and are the gateway working as
designed. Listed so they are not re-opened.

| Class | Why it is correct |
|---|---|
| `MODEL_MODALITY_MISMATCH` | The gateway refusing a chat request to an image model, and the reverse. The guard compares the model's `type` against the kind of endpoint addressed, on both the explicit-model path and the resolver's rule paths. |
| Context length exceeded | The caller's conversation exceeded the model's window. Truncating it would change what the request means. |
| `The function name '<name>' is reserved` | Renaming a caller's tool would break their own dispatch loop. |
| Embedding batch above the vendor cap | Splitting it silently would re-index the caller's vectors against their inputs. |
| 401 / 403 | Virtual-key authentication and model-access policy doing their job. |
| 499 | The client hung up before the upstream answered. |

## References

- `packages/ai-gateway/internal/providers/specs/anthropic/codec/codec.go` — the `max_tokens` fill and its catalog ceiling
- `packages/ai-gateway/internal/providers/specs/gemini/codec/schema_sanitize.go` — Gemini schema key allow-list
- `packages/ai-gateway/internal/providers/specs/compat/deepseek/rewrites.go` — thinking-mode structural rules
- `packages/ai-gateway/internal/providers/specs/openai/rewrites/rewrites.go` — OpenAI sampling and `max_tokens` rules
- `packages/ai-gateway/internal/providers/specs/compat/moonshot/rewrites.go` — Moonshot fixed-temperature rule
- `scripts/quirk-coverage.config.mjs` — the sampling decision per vendor family, with its probe
- `packages/control-plane-ui/src/pages/ai-gateway/providers/detail/catalog-sync.ts` — the catalog diff behind the sync dialog
- `tools/db-migrate/model-catalog.json` — single source of truth for model ceilings and prices
- `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` — §3a adapter rules
