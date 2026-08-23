# Recovery engine architecture

What happens after the first target fails.

Routing produces a plan: an ordered list of targets, each carrying the rule it came from. This
document is about everything after that — how a failure is classified, which target is tried next
and why, what bounds the whole thing, and what an operator can read afterwards. The plan's
construction is [routing-architecture.md](routing-architecture.md); the walk over it is here.

The engine knows nothing about strategies. It reads the plan and the class of each failure, and
nothing else.

## 1. Failure classes

Every dispatch outcome is classified once, in `classify`, from the adapter's `*ProviderError` or the
raw transport error. There are fourteen classes and a `classCount` sentinel, and the sentinel is
load-bearing: it lets a test assert that every class has been considered, rather than that the
classes someone remembered have been.

Classification is finer than the treatment it maps to, deliberately. A class earns its name twice —
once because it selects a distinct recovery treatment, once because it is what an operator reads on
a traffic row. Two failures that need different diagnoses may not share a name even when the engine
treats them alike, which is why an unrecognised provider code is its own class rather than more
network.

Each class answers four questions, and each answer is a named predicate rather than a branch someone
remembered to write:

| question | predicate | what it decides |
|---|---|---|
| Can any other target help? | `abortsRequest()` | the walk stops; the caller gets this answer |
| Is this target out for this request? | `eliminatesTarget()` | the target is never selected again |
| Is this evidence about the API key? | `chargesCredential()` | the credential breaker counts it |
| Is this evidence about the provider? | `recordsProviderHealth()` | the health tracker counts it |

The last two are different questions and they disagree. A `403` says the key is accepted and this
model is not licensed to it: the provider did refuse the call, so it hears about the refusal, and
the key was never at fault, so the breaker does not. Collapsing them loses exactly that case.

The classes that say nothing about the provider are the ones whose failure happened on our side of
the call or on the caller's — a decode fault after the upstream answered and billed is ours; a
malformed body, a request no candidate can serve, and a caller who hung up are the caller's. Counting any of them pushes a
provider that did its job toward unavailable, and the traffic then routed away belongs to everyone
using it.

Three groups, by whose fault the failure is:

- **The REQUEST's** — `bad request`, `no candidate`, `client gone`. No other target would answer
  differently, so the walk ends.
- **This TARGET or PROVIDER's** — `auth failed`, `permission denied`, `target unsupported`,
  `context overflow`, `local processing`, `provider quota exhausted`. Permanent for this request;
  retrying in place spends a call to learn what is already known. An `auth failed` eliminates the
  whole provider, because every target on it shares the credential that was refused, and
  `provider quota exhausted` eliminates at the same scope for the same reason one level over: the
  budget belongs to the account, so every model behind it is equally unusable. It is deliberately
  not in the deprioritise group with `429`: a rate limit clears in seconds, which is why that class
  wants elapsed time, while a spent budget clears when the billing window resets or the customer
  raises the limit, so only a different provider answers. Several upstreams file it under a status
  that says otherwise — Anthropic returns HTTP 400 `invalid_request_error` — so the per-provider
  normaliser reclassifies on the message before the walk ever sees it.
- **The upstream's current state** — `network`, `timeout`, `429`, `5xx`, `unclassified`. Another
  turn may succeed.

The name a class reports is the canonical `ProviderError.Code` string wherever the two describe one
failure, so `code` and `errorClass` on the same trace entry are one word rather than two spellings.
Three classes have names of their own because the code cannot express them: `permission_denied`
(401 and 403 both normalise to `auth_failed`), `network` (a transport error produces no
`ProviderError` at all), and `unknown_provider_code`.

## 2. Choosing the next target

Position is the wrong answer for two classes, and the right answer for the rest.

A list ordered by price puts the next-cheapest model next. After a context overflow that model's
window is as likely to be smaller as larger, so a walk can overflow several times in a row; after a
rate limit the next entry is very often on the SAME provider, whose quota is exactly what was just
exhausted, because a plan built from one provider's catalogue lists that provider's models together.

So `selectNext` branches on the last class and records why:

| reason | when | rule |
|---|---|---|
| `largest-window` | after a context overflow | the untried target with the largest declared window; an undeclared window sorts behind every stated one but ahead of nothing |
| `different-provider` | after 429 / 5xx / timeout / network / unclassified | the first untried target on a provider other than the one that just failed |
| `next-in-list` | everything else | the first untried target — the order the strategy stated, which nothing about this failure argues with |
| `deprioritised-retry` | the resting pass | the rested target left longest: fewest turns first, then list order |

Every clause is scoped to the CURRENT RULE, which is the lowest rule index still holding a target
that has not been ELIMINATED. An attempted target still holds its rule — it has to, or a rested
retry could never re-enter one. Rule indices are assigned by first appearance in the
plan, because the plan already carries the rules in priority order and re-deriving that order here
would be a second answer to which rule outranks which.

"Exhausted" means ELIMINATED, never out of budget. A rule whose targets are merely expensive in
attempts has not been ruled out, and advancing past it because the call budget ran low would hand
the request to a rule the admin wrote for a different situation while the intended one still had
answers.

A target that failed transiently is DEPRIORITISED, not finished. What that buys is elapsed time:
same-target retries happen in milliseconds, while coming back after trying two other providers means
several upstream round-trips have passed — which is what a rate limit needs and what an in-place
retry cannot give it.

That is why two mechanisms answer "try this target again" and both are kept: the retry loop inside a
turn, and the walk coming back later. Which one runs is decided by the class, through
`wantsElapsedTime()`. A rate limit is handed straight back to the walk without an in-place retry,
because another dispatch milliseconds from now meets the same exhausted quota. Everything else
retries in place, because a reset connection or a single 5xx is exactly what an immediate second
attempt fixes — and going elsewhere would let a different model answer a request the intended one
would have served.

What is NOT duplicated is the bound. Both mechanisms draw from one per-target allowance, described
next.

## 3. The two ceilings

Spend and patience fail in different directions, so each has its own bound. Both are read through
one predicate and checked before every dispatch, including the same-target retries.

**The call budget** bounds upstream calls for one request, counting every dispatch across every
target and every rule. Calls are the unit because calls are what cost money: counting rounds would
make one configured number mean wildly different spend depending on how many targets a rule carries,
and exempting same-target attempts would let a budget of 1 permit five billed generations.

Unset — which is how it ships — the budget is derived: `targets × attempts-per-target`. That is also
the most the walk can spend at any setting, because each target's allowance is its own and no other
target can spend what it leaves behind. A configured value is clamped to `[1, 20]` and takes
attempts away; a value above the derived one buys nothing, since no target has one left to give.

**The knobs are ordered, and the per-target one comes first.** `maxAttemptsPerTarget` is how many
times ONE target may be dispatched to on this REQUEST — not within one turn. Both mechanisms in the
previous section draw from that single allowance, so a turn the walk grants later cannot hand a
target a fresh one. A rule capped at two attempts per target takes two, whatever the request budget
says.

**The walk deadline** bounds wall-clock time across the whole walk, defaulting to what a single slow
upstream call gets — the same number as the upstream per-attempt timeout. It is an admission gate,
not a hard stop: it is read before a dispatch, never during one, so a walk admitted just inside the
deadline can exceed it by one full attempt. A walk that has already spent that has not been unlucky; it has been walking
through something that is not going to answer. Without it, a per-attempt timeout measured in minutes
lets a walk over several targets keep dispatching for hours against a client that left long ago —
each dispatch billable, none deliverable.

When either ceiling stops the walk, every target that never got a turn is recorded with the reason
it never got one. A target that vanishes from the record is the thing an operator is most often
trying to account for.

The resting pass needs no opt-in flag guarding it. The flag it replaces existed to stop a survivor
absorbing an eliminated target's slack; a per-target allowance makes that impossible by
construction, so what a target may come back and spend is only ever its own.

## 4. Same-target retries

A target is ATTEMPTED up to `maxAttemptsPerTarget` times for the whole REQUEST, clamped to `[1, 5]`
— so the shipped 2 is one further attempt, not two. The loop and the walk share that count, so a
target that spent it inside its first turn is not selected again, and a target that gave its turn
back keeps what it did not spend.

Only classes that do not want elapsed time reach the retry tail: every aborting, eliminating and
successful outcome returns or leaves the loop first, and a rate limit is handed back to the walk
before it.

The loop exits early when the class is not in the rule's `retryOn` set, when the parent context's
deadline is within one backoff, or when re-resolving the credential fails. Backoff doubles from the
configured initial value, clamps at the configured maximum, and is jittered by a configured
fraction. Every retry after the first re-resolves the credential, so a key whose circuit opened
between attempts is not reused.

`walkState.attempts` counts TURNS, not dispatches: one turn runs the retry loop, which may dispatch
several times.

The pause before a dispatch is sized by a separate count: how many pauses this TARGET has already
served, across every turn it has had. One target, one escalating schedule. An index that counted
position within a turn would start over each time the target was selected again, handing a target
the walk had come back to a SHORTER pause than the ones it had already outlasted.

## 5. What lands on the traffic row

`traffic_event.routing_trace` carries the walk beside the plan. Each attempt entry records the
sequence number, the provider and model, whether it was DISPATCHED, the upstream status and
canonical code, the selection reason, the error class, the latency, the error text, and the request
fields the adapter REWROTE before that dispatch.

The rewrites are per-attempt because a coercion is per-target: the same request translated for two
wires is rewritten differently, so one list per request would attribute the third target's
translation to the first. They are recorded on failed dispatches too — "we rewrote this field and
then it 400ed" is the shape of the question — and the response header that also reports them
reaches a caller who has usually discarded it before anyone asks.

`Dispatched` separates a call that reached an upstream from a target that was passed over. Both
belong in the record — the target that never ran is the one an operator is most often accounting for
— but only the first kind cost anything. An entry for a target that never ran carries its target and
its error text, never a status, code, class, latency or credential.

When a ceiling ends the walk, the entries it writes come in two kinds and the difference between
them is the point. One target had just been SELECTED: its entry says `stopped`, and carries the
reason it was reached for. Every other target never got a turn: those say `skipped`, and carry NO
selection reason, because nothing selected them and a reason computed for a different target would
assert a decision nobody made about this one. The selected target is recorded on its own rather than
through a never-attempted filter, so a REVISIT — a rested target chosen for another turn and stopped
before it dispatched — is on the record too; a filter cannot see it, because its earlier turn already
moved its attempt count off zero.

One limit remains, worth knowing before reading a trace as complete: a target armed as a
context-upgrade escape, when it is passed over because the previous failure was not an overflow, is
recorded nowhere — it is marked accounted-for internally without an entry.

The chain is recorded whether the request succeeded or failed. Selection is not positional, so a
chain that jumps over three entries to reach the fourth is either a deliberate move — an overflow
reaching for the largest window, a rate limit stepping off a provider — or a bug, and from the plan
alone those look identical.

## 6. What the caller gets when nothing succeeded

The LAST dispatched attempt's outcome is the answer, not the last terminal one. Keeping the last
terminal failure returns an early target's envelope — an overflow 400 surfacing over a rate limit,
which no SDK retries — and pairs it with another attempt's credential on the audit row.

Whether an attempt carries an envelope at all is `surfacesUpstreamEnvelope()`: the aborting and
eliminating classes do, because the upstream's own status, headers and body are the right thing to
return when that class ends the walk. The deprioritise classes do not, so a walk that ends on a
429 reports that every target was exhausted, and the handler derives the client's answer from the
last dispatched attempt.

A cancelled client is its own case: the walk stops, no provider health is recorded, and the response
is synthesised rather than taken from an upstream that was never at fault.

## References

- `packages/ai-gateway/internal/execution/executor/classify.go` — the classes and their predicates
- `packages/ai-gateway/internal/execution/executor/select_next.go` — which target is tried next
- `packages/ai-gateway/internal/execution/executor/executor.go` — the walk, the ceilings, the retry loop
- `packages/ai-gateway/internal/execution/executor/walkstate.go` — per-target state and rule scoping
- `packages/ai-gateway/internal/execution/executor/attempt.go` — dispatch, health and credential recording
- `packages/ai-gateway/internal/execution/executor/dispatch_order.go` — the ordering observation
- `packages/shared/schemas/configtypes/policy/retry_policy.go` — the knobs and the derived budget
- `packages/ai-gateway/internal/ingress/proxy/routing_audit_trace.go` — the attempts projection
- `packages/ai-gateway/internal/platform/store/health.go` — the provider health tracker
- `packages/ai-gateway/internal/credentials/stats/buffer.go` — the credential breaker
