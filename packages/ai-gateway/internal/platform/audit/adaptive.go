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
	// auditMemFraction is the share of AVAILABLE memory the audit in-memory queue may
	// pin, as a BYTE budget (see adaptiveMemBudgetBytes + shared/core/bytebudget). ~15%
	// of RAM — the same order as the NATS JetStream memory-store limit, a side-path
	// minority slice. A producer Acquires its record's REAL byte weight before enqueuing
	// and blocks when this budget is exhausted (back-pressure to the request path), so
	// the in-memory audit heap is bounded by BYTES (the actual record size), never by a
	// slot count derived from an ASSUMED body size (the old sizing OOM-killed the process
	// at 18 GiB when real bodies exceeded the assumed average). Configure it below the
	// machine's real headroom (e.g. 15% of RAM) so the soft budget's small overshoot
	// never matters.
	auditMemFraction = 0.15

	// recChStructuralCap / spillChStructuralCap are FIXED structural channel depths —
	// how many *Record POINTERS the queues may hold. NOT the memory bound (the
	// byteBudget bounds bytes); a slot is ~8 B, so a deep channel is cheap. Sized
	// generously so the byte budget binds first for realistic large-body traffic, while
	// the count still caps a pathological flood of tiny records. Body-size-INDEPENDENT
	// on purpose: the memory limit is the byte budget, computed from the ACTUAL record.
	recChStructuralCap   = 200_000
	spillChStructuralCap = 100_000

	// fixedBudgetFallback is the byte budget used when MemAvailable cannot be read
	// (non-Linux / restricted /proc).
	fixedBudgetFallback = 2 << 30 // 2 GiB
)

// adaptiveMemBudgetBytes is the in-memory audit BYTE budget — a fraction of the live
// MemAvailable (fallback fixedBudgetFallback off-Linux). The byteBudget back-pressures
// Enqueue on this, bounding the audit heap by ACTUAL bytes regardless of body size.
func adaptiveMemBudgetBytes() int64 {
	avail := availableMemoryBytes()
	if avail == 0 {
		return fixedBudgetFallback
	}
	return int64(float64(avail) * auditMemFraction)
}

// adaptiveBufferCaps returns the STRUCTURAL channel depths (pointer counts) for
// recCh / spillCh. These are a count ceiling, not the memory bound — the byteBudget
// bounds bytes. Fixed and body-size-independent by design.
func adaptiveBufferCaps() (recCh, spillCh int) {
	return recChStructuralCap, spillChStructuralCap
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
