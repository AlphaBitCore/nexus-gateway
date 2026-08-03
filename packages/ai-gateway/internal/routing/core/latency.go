package core

// LatencyTarget is a leaf provider+model candidate for the latency strategy.
// It carries no weight: the latency strategy orders targets by observed p95,
// not by an operator-assigned weight, which is what distinguishes it from
// ABTarget. It is also not a recovery entry (FallbackChainEntry). A dedicated
// type keeps the config self-documenting — one struct for one concept. The
// StrategyNode.LatencyTargets field that carries these lives in types.go.
type LatencyTarget struct {
	ProviderID string `json:"providerId"`
	ModelID    string `json:"modelId"`
}

// MaxLatencyTargets caps the number of targets a latency strategy node resolves
// per request. The latency strategy resolves and ranks EVERY listed target on
// each request (unlike ab_split, which resolves only the one selected), so the
// per-request lookup+sort fan-out is bounded here against a pathological or
// mis-pushed config. 32 is far above the realistic "one model across N
// providers" fan-out; the overflow is trace-dropped at unmarshal.
const MaxLatencyTargets = 32

// LatencyStatsFunc returns the windowed p95 latency (over successful in-window
// samples, nearest-rank) and the TOTAL in-window sample count (success+failure)
// for a provider. Injected into the latency strategy so the strategy does not
// import the platform/store package directly. A nil func (no health tracker
// wired) makes every target read as cold (total 0), degrading the strategy to a
// safe random load-balance rather than stranding a request. A p95Ms of -1
// signals a warm target that has no successful in-window sample (all failures),
// so the caller can sort it last among warm targets; the health band demotes it
// regardless.
type LatencyStatsFunc func(providerID string) (p95Ms int, total int)
