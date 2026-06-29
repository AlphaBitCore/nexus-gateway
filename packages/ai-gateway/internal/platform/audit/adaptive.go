package audit

// adaptive.go — runtime self-tuning for the audit side-path. The disk's
// read/write bandwidth is a FIXED property of the box; CPU and memory are
// CONTENDED at runtime. Hard-coded buffer / batch magic numbers make performance
// fragile — a machine swap or a config edit can collapse throughput. Instead these
// helpers derive the tuning from the live machine: in-heap + spill buffer capacity
// scales to AVAILABLE MEMORY, and the spill flush size adapts to MEASURED disk
// write latency (write slow → accumulate larger, write fast → flush sooner), so
// the worker automatically rides just under the disk's real write bandwidth
// without ever being told what that bandwidth is. No operator tuning required.

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// meminfoPath is the kernel memory-stats file availableMemoryBytes parses. It is a
// package var (not a literal) solely so a test can point it at a fixture file and
// exercise the parse path on a non-Linux host where /proc/meminfo does not exist.
// Production never reassigns it.
var meminfoPath = "/proc/meminfo"

// availableMemoryBytes reports the kernel's MemAvailable (Linux). 0 when it cannot
// be read (non-Linux / restricted /proc) — callers then fall back to a conservative
// fixed budget rather than guessing the machine wrong.
func availableMemoryBytes() uint64 {
	f, err := os.Open(meminfoPath)
	if err != nil {
		return 0
	}
	defer f.Close() //nolint:errcheck
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line) // "MemAvailable:" "<kB>" "kB"
		if len(fields) >= 2 {
			if kb, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				return kb * 1024
			}
		}
		return 0
	}
	return 0
}

const (
	// auditMemFraction is the share of AVAILABLE memory the audit side-path's
	// in-heap + spill-handoff buffers may occupy. Audit is a side-path, so it takes
	// a minority slice; the rest stays for the request path + page cache.
	auditMemFraction = 0.25

	// avgRecordPinBytes is the assumed memory a queued record pins until it is
	// marshaled out of the pools — dominated by the captured request/response body
	// (the inline cap is 256 KiB, but typical bodies + the small-record mix average
	// far lower). Used only to turn a byte budget into a channel capacity; the exact
	// value is not load-bearing (it sizes a buffer, not a correctness bound).
	avgRecordPinBytes = 48 << 10 // 48 KiB

	// in-heap : spill-handoff split of the buffer budget. The in-heap queue is the
	// fast path (workers drain it to NATS); the spill channel is the overflow runway
	// to disk. Weight the fast path heavier.
	recChBudgetShare   = 0.6
	spillChBudgetShare = 0.4

	// Capacity clamps so a tiny box still gets a usable buffer and a huge box does
	// not allocate an absurd channel. These bound the AUTO value, they are not the
	// value — the machine picks within the band.
	minRecChCap   = 2000
	maxRecChCap   = 2_000_000
	minSpillChCap = 1000
	maxSpillChCap = 1_000_000

	// fixedBudgetFallback is used when MemAvailable cannot be read (non-Linux).
	fixedBudgetFallback = 2 << 30 // 2 GiB
)

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// adaptiveBufferCaps computes the in-heap (recCh) and spill-handoff (spillCh)
// channel capacities from available memory — replacing the former fixed
// maxQueueSize / spillChanCap magic numbers. A bigger box automatically gets
// deeper buffers (more burst absorption before any back-pressure), a smaller box
// gets shallower ones, with no config.
func adaptiveBufferCaps() (recCh, spillCh int) {
	budget := availableMemoryBytes()
	if budget == 0 {
		budget = fixedBudgetFallback
	}
	budget = uint64(float64(budget) * auditMemFraction)
	recCh = clampInt(int(float64(budget)*recChBudgetShare)/avgRecordPinBytes, minRecChCap, maxRecChCap)
	spillCh = clampInt(int(float64(budget)*spillChBudgetShare)/avgRecordPinBytes, minSpillChCap, maxSpillChCap)
	return recCh, spillCh
}

// adaptiveSpillFlush adapts the spill worker's flush size to measured disk write
// latency, so the worker writes in batches that ride just under the disk's real
// bandwidth without being told what it is. Fast writes → small flushes (low
// latency to durability); slow writes (disk busy) → larger flushes (fewer, bigger
// sequential writes, fewer fsync/IOPS). The fixed magic "flush every N MiB" is
// gone — N is now a feedback loop on the device itself.
type adaptiveSpillFlush struct {
	// targetBytes is the current flush threshold (atomic so the spill worker reads
	// it cheaply each record). Starts mid-band; the feedback loop moves it.
	targetBytes  atomic.Int64
	emaLatencyNs atomic.Int64 // smoothed per-MiB write latency
}

const (
	spillFlushMinBytes = 1 << 20   // 1 MiB floor
	spillFlushMaxBytes = 256 << 20 // 256 MiB ceiling
	spillFlushStart    = 8 << 20   // start mid-band

	// perWriteTargetMs is the wall-time we want a single batched spill write to take.
	// The flush size is steered so each write lands near this: if writes are coming
	// back faster, the disk has headroom → shrink (flush sooner); slower → the disk
	// is the bottleneck → grow (amortize). 50 ms keeps durability latency bounded
	// while still batching hard under load.
	perWriteTargetMs = 50
	flushEMAAlpha    = 0.3
)

func newAdaptiveSpillFlush() *adaptiveSpillFlush {
	a := &adaptiveSpillFlush{}
	a.targetBytes.Store(spillFlushStart)
	return a
}

// threshold is the current flush size the spill worker should accumulate to.
func (a *adaptiveSpillFlush) threshold() int { return int(a.targetBytes.Load()) }

// observe feeds back one completed batched write (its byte size and how long it
// took) and steers the threshold toward perWriteTargetMs of wall-time per write.
func (a *adaptiveSpillFlush) observe(bytesWritten int, took time.Duration) {
	if bytesWritten <= 0 || took <= 0 {
		return
	}
	// Per-MiB latency, smoothed — a stable read of the device's current speed.
	perMiB := float64(took.Nanoseconds()) / (float64(bytesWritten) / float64(1<<20))
	prev := a.emaLatencyNs.Load()
	ema := perMiB
	if prev > 0 {
		ema = (1-flushEMAAlpha)*float64(prev) + flushEMAAlpha*perMiB
	}
	a.emaLatencyNs.Store(int64(ema))
	// Size that would take ~perWriteTargetMs at the current per-MiB latency.
	if ema <= 0 {
		return
	}
	wantMiB := float64(perWriteTargetMs*1e6) / ema
	want := int64(wantMiB * float64(1<<20))
	if want < spillFlushMinBytes {
		want = spillFlushMinBytes
	}
	if want > spillFlushMaxBytes {
		want = spillFlushMaxBytes
	}
	a.targetBytes.Store(want)
}
