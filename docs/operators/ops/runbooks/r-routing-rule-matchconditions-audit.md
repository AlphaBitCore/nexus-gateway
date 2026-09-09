# R routing rule matchConditions audit

The admin API refuses a `smart` routing rule whose `matchConditions` do not pin
what the rule may reach. This is the runbook the rejection message links to:
what the rule is, why it exists, how to author a rule that satisfies it, and how
to audit the rules a deployment already carries.

## The rule

A `smart` rule must carry a non-empty `matchConditions.requestedModelLiterals`,
and no entry may reach every request. Concretely, `POST` and `PATCH` answer 400
with `smart_rule_match_conditions_unsafe` when:

| Shape | Why it is refused |
|---|---|
| `matchConditions` absent, `null`, or `{}` | A rule with no conditions is a catch-all. Every request in the fleet would be handed to the router. |
| `requestedModelLiterals` absent | Other dimensions (`projects`, `virtualKeys`) narrow WHO, not WHICH request. A key's every request would still delegate. |
| `requestedModelLiterals: []` | An empty dimension is not a condition — the matcher skips it, so this is the catch-all again. |
| A blank entry — empty or whitespace-only | The gateway trims the client's `model` at admission and rejects what is left empty, so no request reaches the matcher carrying one. The rule is inert: the operator believes they authored a trigger and has authored nothing. |
| An entry of nothing but `*` — `"*"`, `"**"` | `matcher.MatchGlob` quotes every character except `*`, so an all-stars pattern compiles to `^.*$` and claims the whole gateway. |

Everything else is accepted: any number of keywords, and any keyword the
deployment chooses.

The guard reads the **stored** strategy, so an edit that changes only
`matchConditions` on a rule that is already `smart` is judged as a smart rule. An
edit that changes the strategy away from `smart` in the same request is judged as
the new one.

## Why

A smart rule delegates model choice to a router LLM that reads the prompt. That
is the right answer for a request whose caller asked for it, and the wrong answer
for a request whose caller named a model and expected to get it. A rule that
reaches traffic nobody pointed at it produces decisions with nothing grounding
them, attributed to models the client never asked for — and it does so silently,
because a 200 from a substituted model looks exactly like a 200.

The bar is REACH, not vocabulary. Which word delegates model choice is a
deployment's own decision.

## Authoring a rule

`auto` is the usual first entry because OpenAI-style clients already send it, but
nothing is special about the string:

```jsonc
{
  "strategyType": "smart",
  "matchConditions": {
    "requestedModelLiterals": ["auto", "fast", "cheap"],
    "virtualKeys": ["team-a-*"]          // optional: narrows WHO as well
  }
}
```

Entries are matched with `matcher.MatchGlob` against the raw `model` string
BEFORE the catalogue resolves it, so:

- **A plain word** — `auto`, `fast` — compares exactly. This is the common case.
- **A glob** — `gpt-4-*` — is the only way to write a rule that survives the next
  version-suffixed release (`gpt-4o-2024-11-20`), because the raw string is where
  version suffixes live.
- **A keyword that is also a `Model.code`** is legal but means something
  different: the request now names a real catalogue model, so the virtual key's
  allow-list applies to it and the requested-side audit columns are filled. Prefer
  a keyword the catalogue does not carry unless you specifically want a family
  redirected.

Every non-empty dimension of `matchConditions` is AND'd, so adding `virtualKeys`
or `projects` narrows the rule further — it does not substitute for the literals.

## Auditing existing rules

List every enabled smart rule with the keywords it claims:

```bash
curl -s "$CP_URL/api/admin/routing-rules" -H "authorization: Bearer $TOKEN" \
  | python3 -c "
import json,sys
for r in json.load(sys.stdin)['data']:
    if r.get('strategyType') != 'smart': continue
    mc = r.get('matchConditions') or {}
    print(f\"{r['id']}  enabled={r.get('enabled')}  prio={r.get('priority')}  \"
          f\"literals={mc.get('requestedModelLiterals')}  vks={mc.get('virtualKeys')}\")
"
```

Read the output for two things:

1. **A keyword no client sends.** The rule never fires. Traffic that was meant to
   delegate is being served by whatever rule sits behind it, or by the
   explicit-model passthrough. The routing preview on the rule detail page
   confirms it: a keyword no rule claims is reported as a request that would be
   rejected, not as one the passthrough would serve.
2. **A keyword that overlaps a catalogue `Model.code`.** Requests naming that
   model are being routed by the smart rule rather than served as named. That may
   be intended — it is how a family redirect is written — but it should be
   deliberate.

Rules stored before this guard existed can still hold a shape the API now
refuses; they keep working, and the next `PATCH` that touches their
`matchConditions` will surface the rejection. Fix them at that point rather than
in bulk.

## Confirming a keyword reaches the rule

`traffic_event.model_name` carries the literal the caller sent, so one query
answers "did my keyword travel and which rule claimed it":

```sql
SELECT model_name, routing_rule_id, routed_model_name, status_code, created_at
FROM traffic_event
WHERE source = 'ai-gateway'
  AND model_name = '<keyword>'
ORDER BY created_at DESC
LIMIT 20;
```

A `routing_rule_id` matching your rule proves the keyword reached it. A NULL
`routing_rule_id` with a 4xx means no rule claimed the keyword and the catalogue
does not carry it either — the gateway had nothing to serve the request with.

## Related

- [smart-routing-architecture.md](../../../developers/architecture/services/ai-gateway/smart-routing-architecture.md) — what the strategy does once the rule fires.
- [routing-architecture.md](../../../developers/architecture/services/ai-gateway/routing-architecture.md) §`MatchConditions` — the full dimension list and their AND semantics.
- [ai-gateway-routing.md](../../../users/features/cp-ui/ai-gateway-routing.md) — the admin UI surface for the same fields.
