package modela

import (
	"context"
	"io"
	"testing"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// The correctness fake records everything it is handed — delivered ids, prescan
// contents, confirm contents — because the tests assert on those. Under a
// benchmark that recording IS the measurement: profiling the sustained-prescan
// benchmark attributed 86.2% of allocated bytes to the harness (52.4% to the
// benchmark's own per-call result struct, 19.4% / 14.3% / 7.5% to the fake's
// Deliver / Prescan / Confirm recorders) and 0.62% to the held queue the
// streaming work was aiming at. A reading like that cannot show an engine-side
// improvement of any size.
//
// benchSubstrate is the same substrate with the recorders removed and a single
// shared Approve result, so what remains is the engine. It is deliberately a
// separate type: making the correctness fake conditionally silent would put a
// branch in the thing the tests use to prove behaviour.
type benchSubstrate struct {
	units     []fakeUnit
	idx       int
	prescanFP bool
	approve   *hookcore.CompliancePipelineResult
}

func newBenchSubstrate(units []fakeUnit, prescanFP bool) *benchSubstrate {
	return &benchSubstrate{
		units:     units,
		prescanFP: prescanFP,
		approve:   &hookcore.CompliancePipelineResult{Decision: hookcore.Approve},
	}
}

func (b *benchSubstrate) Next(_ context.Context) (fakeUnit, error) {
	if b.idx >= len(b.units) {
		return fakeUnit{}, io.EOF
	}
	u := b.units[b.idx]
	b.idx++
	return u, nil
}

func (b *benchSubstrate) AppendRedactableText(dst []byte, u fakeUnit) []byte {
	return append(dst, u.text...)
}
func (b *benchSubstrate) UnitBytes(u fakeUnit) int                    { return u.bytes }
func (b *benchSubstrate) ContentBytes(u fakeUnit) int                 { return u.content }
func (b *benchSubstrate) IsDone(u fakeUnit) bool                      { return u.done }
func (b *benchSubstrate) Deliver(_ context.Context, _ fakeUnit) error { return nil }
func (b *benchSubstrate) DeliverTerminal(_ context.Context) error     { return nil }
func (b *benchSubstrate) Prescan(_ []byte) bool                       { return b.prescanFP }

func (b *benchSubstrate) Confirm(_ context.Context, _ string) *hookcore.CompliancePipelineResult {
	return b.approve
}

func (b *benchSubstrate) Escalate(_ context.Context, _ []fakeUnit, _ *hookcore.CompliancePipelineResult) error {
	return nil
}

func (b *benchSubstrate) OnConfirmApproved(_ *hookcore.CompliancePipelineResult) {}
func (b *benchSubstrate) OnApproveEOF()                                          {}
func (b *benchSubstrate) OnError(_ context.Context, err error) error             { return err }

func benchUnits(n int) []fakeUnit {
	units := make([]fakeUnit, n)
	for i := range units {
		units[i] = fakeUnit{id: i, text: "x", bytes: 1, content: 1, done: i == n-1}
	}
	return units
}

// BenchmarkRunEngine_SustainedPrescanFP measures the engine with every prescan
// returning a false positive, so each unit is held, confirmed and released —
// the path the hold window exists for.
func BenchmarkRunEngine_SustainedPrescanFP(b *testing.B) {
	units := benchUnits(2000)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		s := newBenchSubstrate(units, true)
		_ = Run(context.Background(), s, Config{TailWindowBytes: 8, MaxBufferBytes: 1 << 20})
	}
}

// BenchmarkRunEngine_NoPrescanHit is the same stream with prescan never firing:
// nothing is held, so the difference between this and the FP benchmark is what
// holding actually costs. Reading either number alone says nothing about the
// hold path, which is how a 0.62% component came to be described as the target.
func BenchmarkRunEngine_NoPrescanHit(b *testing.B) {
	units := benchUnits(2000)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		s := newBenchSubstrate(units, false)
		_ = Run(context.Background(), s, Config{TailWindowBytes: 8, MaxBufferBytes: 1 << 20})
	}
}

// BenchmarkRunEngine_HarnessFloor drains the same substrate through the same
// unit sequence WITHOUT the engine. Whatever it reports is the floor every
// number above sits on; an engine change is only visible in the difference.
// Without a control arm a harness cost is indistinguishable from the code's.
func BenchmarkRunEngine_HarnessFloor(b *testing.B) {
	units := benchUnits(2000)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		s := newBenchSubstrate(units, true)
		var scratch []byte
		for {
			u, err := s.Next(ctx)
			if err != nil {
				break
			}
			scratch = s.AppendRedactableText(scratch[:0], u)
			_ = s.Prescan(scratch)
			_ = s.Deliver(ctx, u)
			if s.IsDone(u) {
				break
			}
		}
	}
}
