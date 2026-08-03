// Package lossmode holds the audit-overflow policy vocabulary shared by every service that
// emits audit events.
//
// It exists because the policy was private to one service. The AI Gateway defined
// lossModeBlock / lossModeSpill / lossModeDrop / lossModeSpillBlock as unexported constants
// with the resolution rule inline, and the compliance-proxy and the agent each grew their own
// overflow behaviour with no way to select a different one: cp always spooled to NDJSON, and
// the agent always dropped. Behaviourally those ARE two of these modes — they just could not
// be configured. Three parallel spellings of one policy is how three services drift, so the
// vocabulary and its safety rule live here once.
//
// What is NOT here on purpose: the durable sink. Each service keeps its own, because each
// already HAS one and they are not interchangeable — the gateway and the compliance proxy
// spool to NDJSON on local disk, and the agent's encrypted SQLite queue is itself the durable
// store with the in-memory channel only a batching buffer in front of it. This package decides
// WHAT the policy is; the service decides WHERE the overflow goes.
package lossmode

// Mode is an audit-overflow policy.
//
// Ordered here from strongest to weakest durability guarantee, which is also the order the
// safety rule in Resolve depends on.
type Mode string

const (
	// SpillBlock is the no-loss default: on overflow, records go to the durable spool
	// FIRST — making the cheap on-disk spool the primary overflow buffer rather than
	// stalling intake at the small in-memory queue — and the emitting path is
	// back-pressured only when that spool is ALSO saturated. At that point it PARKS
	// rather than dropping, retrying while the recovery drain frees spool space, so
	// ingest self-throttles to the drain rate.
	//
	// Genuinely no-loss whenever a durable sink is wired. When one is NOT wired it must
	// degrade to Block, never to a drop — see WithoutDurableSink.
	SpillBlock Mode = "spillblock"

	// Block is the stricter no-loss variant: back-pressure at the in-memory queue and
	// never touch the spool. No loss and no spool dependency, at the cost of stalling
	// the emitting path sooner than SpillBlock would.
	Block Mode = "block"

	// Spill is durable-but-bounded: an asynchronous spill off the emitting path, with a
	// counted drop only if the spill itself is also saturated.
	Spill Mode = "spill"

	// Drop is the lossy opt-out: a counted, bounded drop on overflow. Maximum throughput,
	// and appropriate only where the audit trail is explicitly not a compliance record.
	Drop Mode = "drop"
)

// Default is the mode an unset or unrecognised configuration resolves to.
const Default = SpillBlock

// Resolve turns a configured string into a Mode.
//
// The load-bearing rule: an empty or unrecognised value resolves to Default, which is no-loss.
// A config typo must never be able to make an audit pipeline silently start dropping records —
// on a compliance product that is a data-loss bug that reports itself as nothing at all. This
// is why Resolve has no error return: there is no failure mode worth surfacing to a caller,
// only a safe answer.
func Resolve(s string) Mode {
	switch Mode(s) {
	case SpillBlock, Block, Spill, Drop:
		return Mode(s)
	default:
		return Default
	}
}

// Lossy reports whether this mode can discard a record that the emitter accepted.
//
// Useful for a startup log line or a diagnostics field: an operator should be able to see that
// their audit pipeline is configured to lose records without reading the mode table.
func (m Mode) Lossy() bool {
	return m == Spill || m == Drop
}

// NoLoss reports whether this mode guarantees no record is discarded, ASSUMING the durability
// this mode depends on is actually present. SpillBlock's guarantee is conditional on a wired
// sink, which is what WithoutDurableSink exists to handle.
func (m Mode) NoLoss() bool {
	return m == SpillBlock || m == Block
}

// WithoutDurableSink returns the mode a service should actually run when the configured mode
// wants a durable spool but none is wired.
//
// SpillBlock degrades to Block: still no-loss, just back-pressuring at the in-memory queue
// instead of at a spool that does not exist. Spill degrades to Drop, because an async spill
// with nowhere to spill IS a drop and pretending otherwise would make the mode name lie.
// Every other mode is unchanged.
//
// A caller that degrades SHOULD say so at startup — "configured spillblock but no durable
// spool is wired, running block" — because the difference is when the emitting path stalls,
// and an operator who chose SpillBlock specifically to avoid early stalls needs to know they
// are not getting it.
func (m Mode) WithoutDurableSink() Mode {
	switch m {
	case SpillBlock:
		return Block
	case Spill:
		return Drop
	default:
		return m
	}
}

// String makes Mode printable in a log attribute without a cast at every call site.
func (m Mode) String() string { return string(m) }

// Action is what a service must DO with one event that overflowed its in-memory buffer.
//
// This exists so the three services share the DECISION and differ only in how they carry it
// out. The decision is four lines of policy that must be identical everywhere — that is the
// architectural consistency requirement. The execution cannot be shared and should not be: the
// durable sink is an NDJSON spool in the gateway and the compliance proxy but the encrypted
// SQLite queue in the agent, and "back-pressure" means blocking on a channel whose bound is a
// per-service tuning. Sharing the switch and not the sink is the seam.
type Action uint8

const (
	// ActionSpool: write the event to the durable sink. If the sink itself refuses because it
	// is saturated, a SpillBlock caller must then back-pressure (park and retry) rather than
	// drop — that is what makes SpillBlock no-loss. A Spill caller counts a drop instead.
	ActionSpool Action = iota

	// ActionBlock: back-pressure the emitting path until the buffer has room. Callers on a
	// host data path MUST bound this — the agent's network-extension path may never stall the
	// user's machine — and count a drop if the bound is hit, which is the one place a
	// no-loss mode can still lose a record and therefore the one place that must be counted
	// and logged.
	ActionBlock

	// ActionDrop: discard the event and count it. The counter is not optional: a dropped
	// audit record leaves no other trace, so an uncounted drop is invisible data loss.
	ActionDrop
)

// String makes an Action readable in a log attribute or a test failure.
func (a Action) String() string {
	switch a {
	case ActionSpool:
		return "spool"
	case ActionBlock:
		return "block"
	case ActionDrop:
		return "drop"
	default:
		return "unknown"
	}
}

// OnOverflow maps this mode to the action a service takes when its in-memory buffer is full.
//
// hasDurableSink reports whether the caller actually has somewhere durable to spool RIGHT NOW —
// not whether it was configured to have one. Passing false folds in the WithoutDurableSink
// degradation, so a service cannot accidentally attempt a spool it has no sink for and then
// have to invent a fallback: SpillBlock without a sink blocks (still no-loss), and Spill without
// a sink drops (honest, since an async spill with nowhere to spill is a drop).
func (m Mode) OnOverflow(hasDurableSink bool) Action {
	switch m.effective(hasDurableSink) {
	case SpillBlock, Spill:
		return ActionSpool
	case Block:
		return ActionBlock
	default: // Drop
		return ActionDrop
	}
}

// effective is the mode actually in force given whether a durable sink is present.
func (m Mode) effective(hasDurableSink bool) Mode {
	if hasDurableSink {
		return m
	}
	return m.WithoutDurableSink()
}
