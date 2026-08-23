package strategies

import (
	"context"
	"fmt"
	"time"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/capability"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/inputstaging"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// defaultOutputReserveTokens is the completion-space reserve used by the
// context-window filter when the request carries no maxTokens: the
// selected model must fit the estimated input plus at least this much
// output. Kept modest on purpose — over-reserving would filter out
// models that could actually serve the request.
const defaultOutputReserveTokens = 1024

// estimateRequestTokens approximates the upstream context cost of the
// full canonical request: text in every role (assistant/tool history
// usually dominates, not the user turn), tool-use input JSON, tool-result
// output, and tool definitions. Image blocks are counted (images) but not
// token-estimated — the character heuristic has no meaningful mapping for
// binary refs, and the filter must stay fail-open on what it cannot
// measure; the count feeds the router's request-metadata line.
//
// It uses the CONSERVATIVE estimate, never the average-case one: this count
// gates filterByContextWindow, where under-counting is the one unrecoverable
// error — it admits a model whose context cannot hold the request, which then
// hard-400s upstream (observed: an auto-routed body counted 216543 tokens
// against a 131072-limit model that the 0.25/char average had sized under
// 128k). Over-counting only selects a larger-context model, which succeeds.
// requestModalities is the set of modalities a request actually carries.
//
// A bitmask rather than a []string: it is built on every auto-routed request
// inside the message loop, and the alternative allocates and then does a
// linear scan per candidate per required modality. Membership here is one AND.
type requestModalities uint8

const (
	reqText requestModalities = 1 << iota
	reqImage
	reqAudio
	reqVideo
	reqFile
)

// modalityBit maps a MediaRef modality onto its bit. An unknown modality
// returns 0, which contributes nothing to the set — a request carrying media
// we do not recognise must not silently satisfy some model's requirement.
func modalityBit(modality string) requestModalities {
	switch modality {
	case normcore.ModalityImage:
		return reqImage
	case normcore.ModalityAudio:
		return reqAudio
	case normcore.ModalityVideo:
		return reqVideo
	case normcore.ModalityFile:
		return reqFile
	}
	return 0
}

func estimateRequestTokens(p *normcore.NormalizedPayload) (tokens, images int, mods requestModalities) {
	for _, m := range p.Messages {
		for _, b := range m.Content {
			tokens += inputstaging.EstimateTokensConservative(b.Text)
			// Only images carry a vision-token cost. The former image_ref
			// block type also held audio and documents, so those inflated
			// this count against models that never charge for them.
			if b.MediaRef != nil {
				mods |= modalityBit(b.MediaRef.Modality)
				if b.MediaRef.Modality == normcore.ModalityImage {
					images++
				}
			}
			if b.Text != "" {
				mods |= reqText
			}
			if b.ToolUse != nil {
				tokens += inputstaging.EstimateTokensConservative(b.ToolUse.Name)
				if len(b.ToolUse.Input) > 0 {
					if j, err := json.Marshal(b.ToolUse.Input); err == nil {
						tokens += inputstaging.EstimateTokensConservative(string(j))
					}
				}
			}
			if b.ToolResult != nil {
				tokens += inputstaging.EstimateTokensConservative(b.ToolResult.Output)
			}
		}
	}
	for _, td := range p.Tools {
		tokens += inputstaging.EstimateTokensConservative(td.Name)
		tokens += inputstaging.EstimateTokensConservative(td.Description)
		if len(td.ParametersJSONSchema) > 0 {
			if j, err := json.Marshal(td.ParametersJSONSchema); err == nil {
				tokens += inputstaging.EstimateTokensConservative(string(j))
			}
		}
	}
	return tokens, images, mods
}

// filterByContextWindow keeps candidates whose declared context window
// holds the required token estimate. Candidates without a declared
// MaxContextTokens pass (fail-open: the filter only acts on data the
// catalog has). When no candidate fits, the largest-context candidates
// are kept instead of emptying the pool — the estimate is coarse, and
// the largest model gives the request its best chance, whereas falling
// back to a (typically small) default would guarantee the overflow.
func filterByContextWindow(candidates []core.SmartModelRow, required int) (kept []core.SmartModelRow, dropped int, noneFit bool) {
	for _, c := range candidates {
		if c.MaxContextTokens == nil || *c.MaxContextTokens >= required {
			kept = append(kept, c)
		}
	}
	if len(kept) > 0 || len(candidates) == 0 {
		return kept, len(candidates) - len(kept), false
	}
	// Every candidate declared a too-small window (a nil MaxContextTokens
	// would have been kept above). Keep the largest tier, ties included.
	maxCtx := 0
	for _, c := range candidates {
		if *c.MaxContextTokens > maxCtx {
			maxCtx = *c.MaxContextTokens
		}
	}
	for _, c := range candidates {
		if *c.MaxContextTokens == maxCtx {
			kept = append(kept, c)
		}
	}
	return kept, len(candidates) - len(kept), true
}

// Tags a candidate must declare when the request provably needs the
// capability, each read from the field that owns it.
const (
	featureFunctionCalling = "function_calling"
	// A model that reasons before answering. The catalogue spells it
	// `reasoning`; `thinking` was a second name for the same capability on a
	// disjoint set of vendors, and it is gone.
	featureReasoning = "reasoning"
	// A model that will hold its answer to a caller-supplied JSON Schema.
	//
	// Deliberately NOT the pre-existing `json_mode` tag, which describes the
	// weaker `response_format: {type: json_object}` and disagrees with the wire
	// on exactly the models that matter: probed per model on 2026-08-19,
	// gpt-4-turbo CARRIES json_mode yet answers 400 to a json_schema, while all
	// ten claude-*, all five command-* and the o-series carry no json_mode and
	// serve structured outputs perfectly. Reusing it would have excluded
	// Anthropic, Cohere and the o-series from every structured-output request —
	// an enumeration refusing what the provider serves, which is the failure
	// this program spent its first half removing.
	featureStructuredOutputs = "structured_outputs"
	// The modality names as a CATALOG row spells them in InputModalities —
	// deliberately not the "vision" feature, which describes the same thing in
	// a second vocabulary. Features keeps the non-modality capabilities.
	//
	// Spelled here rather than reused from normcore because they are a
	// different vocabulary that merely happens to agree: normcore's values
	// describe a MediaRef on the request, these describe a column in the model
	// catalog. The agreement is what makes the filter work at all, so it is
	// pinned by a test rather than assumed by a shared constant that would hide
	// a divergence the day one side moves.
	modalityImage = "image"
	modalityAudio = "audio"
	modalityVideo = "video"
	modalityFile  = "file"
)

// carriedModalities pairs each media modality bit with the name a catalog row
// spells in InputModalities, so the acceptance filter can ask its question once
// per modality the request actually carries.
//
// Text is absent on purpose: it is not a MediaRef modality, every catalog row
// declares it, and a filter that can never drop anything is a pass that costs
// the whole pool a scan.
//
// A fixed array rather than a table of closures — the file already measured
// that shape at 11% slower on this path, because the per-candidate call is an
// indirect one the compiler will not inline. Four entries, and the inner loop
// runs only for modalities present in the request, which for an ordinary text
// request is none of them.
var carriedModalities = [4]struct {
	bit  requestModalities
	name string
}{
	{reqImage, modalityImage},
	{reqAudio, modalityAudio},
	{reqVideo, modalityVideo},
	{reqFile, modalityFile},
}

// filterByCapability keeps candidates that can serve the request's provable
// needs: image input when the request carries image blocks, function_calling
// when it declares tools. Each dimension reads the field that owns it —
// modalities from InputModalities, capabilities from Features — so no property
// is described twice. Only candidates that DECLARE the relevant list are
// judged; an empty list passes (fail-open: absent catalog metadata must not
// disqualify). A dimension that would empty the pool is skipped entirely
// (fail-open); skipped dimension names are returned so the caller can trace
// them.
// Each dimension is written out rather than driven from a table of closures.
// The table read better, and it measured 11% slower on the routing path — the
// per-candidate call is an indirect one the compiler will not inline. Two
// explicit passes cost nothing and this sits on every auto-routed request.
func filterByCapability(candidates []core.SmartModelRow, needsTools, needsReasoning, needsStructuredOutput bool, have requestModalities, carried []string) (kept []core.SmartModelRow, dropped int, skippedDims []string) {
	kept = candidates
	// Acceptance, asked once per modality the request actually carries.
	//
	// This used to ask about images only, gated on an estImages counter, and its
	// own comment claimed the dimensions above the floor ask "can this candidate
	// take what the request carries" — which was true of exactly one of them. A
	// request carrying audio, video or a document was routed without anyone
	// asking whether the chosen model accepts it, so the router itself produced
	// the upstream 400. When the CALLER names a model, the model's limits are
	// the model's; when WE pick it, a refusal caused by our pick is ours.
	//
	// Reading the modality arrays, never features["vision"]: the two described
	// the same property and disagreed on 34 production rows — every one
	// advertising vision while declaring text-only input — so a router that
	// consults the feature list sends an image to a model that cannot read it.
	//
	// The pool-emptying fail-open below is load-bearing for documents in
	// particular: no catalog row declares "file" today, so that dimension would
	// otherwise drop every candidate on every document request.
	//
	// It is a POOL rule, not a per-candidate one, and that is what makes it
	// right. A row declaring ["text"] IS saying it will not take an image, so
	// it should be dropped from an image request — as long as something in the
	// pool survives to prove the dimension had an opinion at all. When nothing
	// survives, the uniform absence is about the catalog rather than about any
	// row, and the dimension is dropped instead of the pool. Replacing this
	// with a per-candidate "did anyone declare it" test keeps the text-only row
	// on an image request, which is the failure this filter exists to stop.
	for _, m := range carriedModalities {
		if have&m.bit == 0 {
			continue
		}
		var pass []core.SmartModelRow
		for _, c := range kept {
			if capability.DeclarationAcceptsModality(c.InputModalities, m.name) {
				pass = append(pass, c)
			}
		}
		if len(pass) == 0 {
			skippedDims = append(skippedDims, m.name)
			continue
		}
		dropped += len(kept) - len(pass)
		kept = pass
	}
	// The floor. The two dimensions above ask "can this candidate take what the
	// request carries"; this one asks the opposite — does the candidate need
	// something the request does not have. gpt-audio-mini is type=chat and
	// accepts text, so it lands in the pool for every plain text request and
	// then 400s upstream, sixteen times before this existed. Empty
	// RequiredModalities is the normal case and costs one length check.
	//
	// One pass, and it only allocates when something is actually dropped. 198
	// of 200 catalog models have no floor, so the filter's job on almost every
	// request is to change nothing — building a fresh slice to do that copied
	// the whole pool and measured +98% with double the allocations. Scanning
	// for the first failure instead lets the common case return the input
	// slice untouched, and `len == 0` short-circuits before requiredBits is
	// ever called.
	if i := firstFloorMiss(kept, carried); i >= 0 {
		pass := make([]core.SmartModelRow, i, len(kept))
		copy(pass, kept[:i])
		for _, c := range kept[i+1:] {
			if capability.DeclarationMeetsFloor(c.RequiredModalities, carried) {
				pass = append(pass, c)
			}
		}
		// No escape here, unlike the acceptance dimensions above.
		//
		// A ceiling that empties the pool is evidence about the CATALOGUE — no
		// row declares `file`, so the dimension has no opinion and enforcing it
		// would refuse every document request. A floor is never that: it says
		// this candidate needs something the request does not carry, which is a
		// fact about the candidate and stays true however many others share it.
		// A pool of nothing but audio-requiring models cannot serve a text
		// request, and saying so is the true answer — keeping them routes the
		// request to a model that answers 400, which is what the floor was
		// written for after sixteen of them.
		//
		// The recovery-side filter has always read it this way. Two answers to
		// one question is the defect; this is the half that was wrong.
		dropped += len(kept) - len(pass)
		kept = pass
	}
	// Tool use is not a modality, so it keeps reading the feature list.
	if needsTools {
		var pass []core.SmartModelRow
		for _, c := range kept {
			if len(c.Features) == 0 || containsFeature(c.Features, featureFunctionCalling) {
				pass = append(pass, c)
			}
		}
		if len(pass) == 0 {
			skippedDims = append(skippedDims, featureFunctionCalling)
		} else {
			dropped += len(kept) - len(pass)
			kept = pass
		}
	}
	// Reasoning, the same shape as tools: the request declares an intent and
	// the model has to declare the capability.
	//
	// It is an ELIGIBILITY constraint and not a preference. A caller who asked
	// a model to think and got an answer from one that cannot has not been
	// given a cheaper result — they have been given a different KIND of
	// result, silently, and the request that produced it looks identical on
	// the row to one that was served properly.
	if needsReasoning {
		var pass []core.SmartModelRow
		for _, c := range kept {
			if len(c.Features) == 0 || containsFeature(c.Features, featureReasoning) {
				pass = append(pass, c)
			}
		}
		if len(pass) == 0 {
			skippedDims = append(skippedDims, featureReasoning)
		} else {
			dropped += len(kept) - len(pass)
			kept = pass
		}
	}
	// Structured output, the same shape again — and the one where a wrong pick
	// does not announce itself. gpt-4-turbo and the DeepSeek family answer a
	// json_schema request with a loud 400, which a 4xx terminates
	// (classNoFailoverNoRetry) rather than failing over onto something worse.
	// kimi-k2.5 does not: probed 2026-08-19 it accepts the field, returns
	// HTTP 200 with finish_reason=stop, and answers in prose. The caller's
	// json.loads is the first thing that notices. Passthrough cannot make that
	// honest — only not choosing it can.
	if needsStructuredOutput {
		var pass []core.SmartModelRow
		for _, c := range kept {
			if len(c.Features) == 0 || containsFeature(c.Features, featureStructuredOutputs) {
				pass = append(pass, c)
			}
		}
		if len(pass) == 0 {
			skippedDims = append(skippedDims, featureStructuredOutputs)
		} else {
			dropped += len(kept) - len(pass)
			kept = pass
		}
	}
	return kept, dropped, skippedDims
}

// firstFloorMiss returns the index of the first candidate whose floor the
// request does not meet, or -1 when every candidate passes. Returning an index
// rather than a bool lets the caller keep the prefix it already verified
// instead of re-testing it.
func firstFloorMiss(rows []core.SmartModelRow, carried []string) int {
	// Indexed, not ranged: SmartModelRow is wide enough that `for _, r := range`
	// copies it per iteration, and this loop runs over the whole candidate pool
	// on every auto-routed request. Measured on the image-need benchmark:
	// 2096ns ranged, 1722ns indexed.
	for i := range rows {
		if req := rows[i].RequiredModalities; len(req) > 0 &&
			!capability.DeclarationMeetsFloor(req, carried) {
			return i
		}
	}
	return -1
}

func containsFeature(features []string, want string) bool {
	for _, f := range features {
		if f == want {
			return true
		}
	}
	return false
}

// armContextUpgrade returns the picked target, plus — when a candidate
// in the same filtered (VK- and capability-respecting) pool declares a
// strictly larger context window — the largest such candidate marked
// ContextUpgradeOnly. The size estimate feeding the context filter is
// coarse, so the pick can still overflow upstream; the executor fails
// over to the armed target exactly on a context-overflow verdict and
// never for transient failures.
func (s *SmartStrategy) armContextUpgrade(ctx context.Context, candidates []core.SmartModelRow, selected *core.SmartModelRow, targets []core.RoutingTarget, trace *[]core.TraceEntry, start time.Time) []core.RoutingTarget {
	if selected.MaxContextTokens == nil {
		return targets
	}
	// A model the re-selection pool already carries is reachable for every
	// failure, not only an overflow. Arming a second, restricted copy of it
	// would put the same target in the plan twice and spend budget derived
	// from a length that counts it twice.
	inPlan := make(map[string]bool, len(targets))
	for i := range targets {
		inPlan[targets[i].ModelID] = true
	}
	var up *core.SmartModelRow
	for i := range candidates {
		c := &candidates[i]
		if c.ModelID == selected.ModelID || c.MaxContextTokens == nil || inPlan[c.ModelID] {
			continue
		}
		if *c.MaxContextTokens > *selected.MaxContextTokens && (up == nil || *c.MaxContextTokens > *up.MaxContextTokens) {
			up = c
		}
	}
	if up == nil {
		return targets
	}
	upTarget, err := s.deps.Lookup(ctx, up.ProviderID, up.ModelID)
	if err != nil {
		return targets
	}
	upTarget.ContextUpgradeOnly = true
	targets = append(targets, *upTarget)
	*trace = append(*trace, core.TraceEntry{
		StrategyType: "smart",
		Decision:     fmt.Sprintf("context-upgrade armed: %s [%s/%s] (window %d > %d)", core.FormatTargetFriendly(upTarget), up.ProviderID, up.ModelID, *up.MaxContextTokens, *selected.MaxContextTokens),
		DurationMs:   int(time.Since(start).Milliseconds()),
	})
	return targets
}

// requestAsksToReason reports whether the CALLER expressed an intent to have
// the model reason — a level, a budget, or a request for the thoughts back.
//
// Read from the canonical rather than from any wire's spelling: the same intent
// arrives as `reasoning_effort`, `thinking.budget_tokens` or a
// `thinkingConfig`, and a router that recognised only one of them would filter
// correctly for callers on one ingress and not the others.
func requestAsksToReason(p *normcore.NormalizedPayload) bool {
	if p == nil || p.Params == nil {
		return false
	}
	return p.Params.Reasoning.Asked()
}
