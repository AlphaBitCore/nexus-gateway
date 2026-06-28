// spill_recovery.go — the background drainer that replays durable spill files
// back into the audit transport. The producer's overflow path writes whole
// records to a rotating NDJSON spool (backpressure.go / writer.go spillData):
// that guarantees no DATA loss, but a spilled record never reaches the
// queryable store on its own. This sweeper closes that gap — it seals the spool,
// re-publishes each sealed file's records to the same MQ queue the live path
// uses, and deletes a file only after every one of its frames is durably acked.
//
// No-loss contract: a record is either in a spool file OR durably accepted by
// the broker (briefly both, during the publish→delete window) — never neither.
// A spool line is the exact wire form the live path publishes (one
// TrafficEventMessage JSON per line), so the Hub consumer ingests a replayed
// record identically and dedupes it by request id — making a re-publish after a
// partial failure idempotent, so the safe response to any uncertainty is to
// leave the file for the next pass rather than risk a drop.
//
// This is the spill-DEFER architecture's drain half: at peak the request path
// pays only a sequential file append (cheap, off the hot path); this sweeper
// moves those records to the broker when the box has headroom, paced to yield to
// the gateway's core request path.
package audit

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// maxPayloadMargin is reserved below the broker's negotiated max_payload for the
// NATS message envelope (subject + protocol headers) when deriving the largest
// record the sweeper will try to publish. A record over (max_payload - margin) is
// dead-lettered instead of attempted.
const maxPayloadMargin = 4 << 10 // 4 KiB

// spillSource is the seal-and-list surface the sweeper needs from the durable
// spool. *sharedndjson.Writer satisfies it; a fake satisfies it in tests.
type spillSource interface {
	// Rotate seals the file currently being appended so it becomes eligible for
	// recovery. A no-op when nothing is open.
	Rotate() error
	// SealedFiles lists the complete spool files (absolute paths), excluding the
	// one currently being appended, oldest first.
	SealedFiles() ([]string, error)
}

// spillRecovery replays sealed spool files into the MQ queue. It owns no state
// beyond its configuration; runOnce is the unit of work and is independently
// testable with a fake producer + a temp spool dir.
type spillRecovery struct {
	src    spillSource
	bp     batchProducer
	queue  string
	logger *slog.Logger

	// frameMaxBytes bounds one published NATS message (mirrors Writer.frameMaxBytes).
	// <= 0 means one record per message.
	frameMaxBytes int
	// batchMaxBytes bounds how many bytes of frames are held + published in one
	// EnqueueBatchAsync, so re-ingesting a large spool file does not materialise the
	// whole file in memory at once. A file larger than this is published in several
	// batches; the file is deleted only if every batch of it acked.
	batchMaxBytes int

	// pace is slept between files so the sweep yields the box to the gateway's
	// core request path (the spill-defer point: drain when there is headroom, do
	// not race the gateway). 0 disables pacing.
	pace time.Duration

	// wireBinary re-frames recovered records for the broker the same way the live
	// writer does: a binary frame (magic + length-prefixed records) when the gateway
	// emits the binary wire, NDJSON otherwise. A binary record re-published as an
	// NDJSON frame would be mis-split by the Hub (its raw bytes can contain 0x0A and
	// it begins with field-id 1 = the frame magic).
	wireBinary bool

	// maxRecordBytes is the largest record the broker can accept (its negotiated
	// max_payload, less a margin for the NATS message envelope). A spilled record
	// larger than this can NEVER publish, so it is dead-lettered to a durable
	// `.poison` sidecar instead of failing its file forever (the oversize-record
	// wedge). 0 means "unknown" → no proactive dead-letter (such a record then just
	// leaves its file for a future pass, the pre-recovery behaviour).
	maxRecordBytes int

	// onReingested / onError / onPoisoned are metric hooks; nil-safe via helpers.
	onReingested func(records int)
	onError      func()
	onPoisoned   func(records int)

	// Seams for fault tests; default to the real filesystem in production.
	openFile     func(string) (io.ReadCloser, error)
	removeFile   func(string) error
	appendPoison func(poisonPath string, rec []byte) error
}

// newSpillRecovery builds a sweeper with production filesystem seams. frameMax and
// batchMax fall back to the Writer-path defaults when non-positive.
func newSpillRecovery(src spillSource, bp batchProducer, queue string, frameMax, batchMax int, pace time.Duration, wireBinary bool, logger *slog.Logger) *spillRecovery {
	if batchMax <= 0 {
		batchMax = batchMaxBytes
	}
	return &spillRecovery{
		src:           src,
		bp:            bp,
		queue:         queue,
		logger:        logger,
		frameMaxBytes: frameMax,
		batchMaxBytes: batchMax,
		pace:          pace,
		wireBinary:    wireBinary,
		openFile:      func(p string) (io.ReadCloser, error) { return os.Open(p) },
		removeFile:    os.Remove,
		appendPoison:  appendPoisonFile,
	}
}

// appendPoisonFile durably appends one un-publishable record (one NDJSON line) to
// path. path is the spool file's name + ".poison" — it does NOT end in ".ndjson",
// so SealedFiles never re-lists it: the record is retained for operator inspection,
// never re-attempted (it can't publish), and never lost.
func appendPoisonFile(path string, rec []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	if _, err := f.Write(append(append([]byte(nil), rec...), '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// maxBackoffMultiple caps how far the sweep interval stretches under sustained
// publish failure (the NATS-full / DiscardNew case), so a wedged broker does not
// make recovery busy-spin re-reading + re-failing the same files every interval.
const maxBackoffMultiple = 16

// run is the sweeper goroutine: rotate+drain on a timer until ctx is done. The
// interval adapts: a clean sweep runs at the base interval; a sweep with publish
// failures (e.g. NATS at MaxBytes rejecting publishes) backs off exponentially up
// to maxBackoffMultiple, so recovery yields the box instead of burning CPU
// re-reading files it cannot drain — then snaps back to base once publishes
// succeed. No-loss is unaffected (files just wait longer). A transient FS/broker
// fault self-heals without operator action.
func (r *spillRecovery) run(ctx context.Context, base time.Duration) {
	cur := base
	t := time.NewTimer(cur)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			failures := r.runOnce(ctx)
			if failures > 0 {
				if cur < base*maxBackoffMultiple {
					cur *= 2
				}
			} else {
				cur = base
			}
			t.Reset(cur)
		}
	}
}

// runOnce seals the active file and drains all currently-sealed files once,
// returning the number of files left undrained by a publish/poison failure (the
// backoff signal for run). It is the testable unit: a fake producer + temp spool
// dir exercises the full read→frame→publish→delete path without a goroutine or a
// live broker.
func (r *spillRecovery) runOnce(ctx context.Context) int {
	if err := r.src.Rotate(); err != nil {
		r.logf("audit: spill recovery rotate failed", "error", err)
		// Continue: already-sealed files from earlier rotations are still drainable.
	}
	files, err := r.src.SealedFiles()
	if err != nil {
		r.logf("audit: spill recovery list failed", "error", err)
		return 1 // treat a list failure as a reason to back off
	}
	failures := 0
	for _, f := range files {
		if ctx.Err() != nil {
			return failures
		}
		if !r.drainFile(ctx, f) {
			failures++
		}
		if r.pace > 0 {
			select {
			case <-time.After(r.pace):
			case <-ctx.Done():
				return failures
			}
		}
	}
	return failures
}

// drainFile re-publishes one sealed spool file and deletes it iff every record was
// durably acked (or durably dead-lettered). On ANY uncertainty (open/read error,
// enqueue error, a per-record nak, poison-write failure, or context cancellation
// mid-file) it leaves the file in place and returns false: the next pass replays
// it and the Hub dedupes the records that already committed, so a partial success
// never double-counts and a failure never drops. Returns true when the file was
// fully resolved and deleted (an empty sealed file is deleted with nothing to
// replay). The bool is run's backoff signal.
func (r *spillRecovery) drainFile(ctx context.Context, path string) bool {
	f, err := r.openFile(path)
	if err != nil {
		r.logf("audit: spill recovery open failed", "file", path, "error", err)
		return false
	}
	defer f.Close() //nolint:errcheck

	sc := bufio.NewReader(f)
	var (
		batch     [][]byte // sealed frames pending one EnqueueBatchAsync
		batchAcc  int      // bytes of sealed frames in batch
		curFrame  []byte   // frame being packed (records + '\n' delimiters)
		records   int      // records re-ingested from this file (durably acked)
		poisoned  int      // records dead-lettered as un-publishable (oversize)
		allAcked  = true
		readError = false
		poisonErr = false
	)

	flushBatch := func() bool {
		if len(curFrame) > 0 { // seal the in-progress frame into the batch
			batch = append(batch, curFrame)
			curFrame = nil
		}
		if len(batch) == 0 {
			return true
		}
		cctx, cancel := context.WithTimeout(ctx, batchPublishTimeout)
		errs, perr := r.bp.EnqueueBatchAsync(cctx, r.queue, batch)
		cancel()
		batch, batchAcc = nil, 0
		if perr != nil {
			r.logf("audit: spill recovery publish failed", "file", path, "error", perr)
			r.errored()
			return false
		}
		for _, e := range errs {
			if e != nil {
				r.logf("audit: spill recovery record nak", "file", path, "error", e)
				r.errored()
				return false
			}
		}
		return true
	}

	// sealFrame moves the in-progress frame into the batch and flushes the batch
	// when it reaches the byte budget.
	sealFrame := func() bool {
		if len(curFrame) == 0 {
			return true
		}
		batch = append(batch, curFrame)
		batchAcc += len(curFrame)
		curFrame = nil
		if r.batchMaxBytes > 0 && batchAcc >= r.batchMaxBytes {
			return flushBatch()
		}
		return true
	}

	for {
		line, rerr := sc.ReadBytes('\n')
		b64 := line // trim the trailing newline; a final unterminated line is still a record
		if n := len(b64); n > 0 && b64[n-1] == '\n' {
			b64 = b64[:n-1]
		}
		if len(b64) > 0 {
			// Spool lines are base64 (binary-safe framing). Decode back to the
			// original marshaled record; a legacy raw-JSON line drains as-is.
			rec, _ := spillDecodeLine(b64)
			// A record larger than the broker's max_payload can never publish — dead-
			// letter it durably instead of failing the file forever (the oversize-
			// record wedge). Only when the cap is known (maxRecordBytes > 0).
			if r.maxRecordBytes > 0 && len(rec) > r.maxRecordBytes {
				if err := r.appendPoison(path+".poison", rec); err != nil {
					r.logf("audit: spill recovery poison write failed", "file", path, "error", err)
					r.errored()
					poisonErr = true
					break // cannot dead-letter → leave the file, do not lose the record
				}
				poisoned++
				continue
			}
			records++
			// Pack rec into the current frame using the WIRE-CORRECT framing (binary =
			// magic + length-prefixed records; JSON = NDJSON), so the Hub decodes a
			// recovered record exactly as it decodes live traffic. A new frame starts
			// when the current one is non-empty and framing is OFF or the record would
			// push it past frameMaxBytes; an over-cap single record ships alone.
			perRecOverhead := 1 // NDJSON '\n'
			if r.wireBinary {
				perRecOverhead = uvarintLen(uint64(len(rec)))
			}
			framingOff := r.frameMaxBytes <= 0
			wouldOverflow := r.frameMaxBytes > 0 && len(curFrame)+len(rec)+perRecOverhead > r.frameMaxBytes
			if len(curFrame) > 0 && (framingOff || wouldOverflow) {
				if !sealFrame() {
					allAcked = false
					break
				}
			}
			// spillDecodeLine returns a fresh slice, so the record is safely owned by
			// the frame's backing array on append.
			if r.wireBinary {
				if len(curFrame) == 0 {
					curFrame = append(curFrame, mq.BinwireMagic)
				}
				curFrame = binary.AppendUvarint(curFrame, uint64(len(rec)))
				curFrame = append(curFrame, rec...)
			} else {
				curFrame = append(curFrame, rec...)
				curFrame = append(curFrame, '\n')
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				r.logf("audit: spill recovery read failed", "file", path, "error", rerr)
				readError = true
			}
			break
		}
	}

	if allAcked && !readError && !poisonErr {
		if !flushBatch() { // publish the final partial frame/batch
			allAcked = false
		}
	}

	if !allAcked || readError || poisonErr {
		return false // leave the file; next pass replays it (Hub dedupes by id)
	}

	// Every record is now either durably acked by the broker or durably dead-
	// lettered to the .poison sidecar — count and delete. reingested is counted
	// even if the delete below fails (the records ARE in the broker); a failed
	// delete only means a replay next pass, which the Hub dedupes by id.
	r.reingested(records)
	r.poisonedRecords(poisoned)
	if err := r.removeFile(path); err != nil {
		r.logf("audit: spill recovery delete failed (records already re-ingested)", "file", path, "error", err)
	}
	return true
}

func (r *spillRecovery) logf(msg string, args ...any) {
	if r.logger != nil {
		r.logger.Warn(msg, args...)
	}
}

func (r *spillRecovery) reingested(n int) {
	if r.onReingested != nil {
		r.onReingested(n)
	}
}

func (r *spillRecovery) errored() {
	if r.onError != nil {
		r.onError()
	}
}

func (r *spillRecovery) poisonedRecords(n int) {
	if r.onPoisoned != nil && n > 0 {
		r.onPoisoned(n)
	}
}
