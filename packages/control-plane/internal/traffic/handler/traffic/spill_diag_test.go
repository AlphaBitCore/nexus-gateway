package traffic

// Spill-read failure diagnosis (S-2).
//
// The silent failure these tests guard: a spilled audit body that cannot be
// fetched used to produce ONE generic "spill body fetch failed" log line for
// every cause. On a multi-host deployment where each node runs its own localfs
// spill root, EVERY spilled body is permanently unreadable from the Control
// Plane — and it looked exactly like a transient S3 hiccup, so an operator had
// no signal that the deployment shape itself was wrong.
//
// Each test therefore asserts the OBSERVABLE an operator acts on: the stable
// cause label on the emitted log record, and — where the two are easy to
// confuse — that the remedy does not send them after the wrong fix.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/trafficstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/localfs"
)

// captureLogs wires a handler's logger to a JSON sink and returns a reader over
// the records it emitted, so a test asserts what the operator actually sees
// rather than an internal return value.
func captureLogs(h *Handler) func(t *testing.T) []map[string]any {
	var buf bytes.Buffer
	h.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return func(t *testing.T) []map[string]any {
		t.Helper()
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("log line is not JSON: %v (%q)", err, line)
			}
			out = append(out, rec)
		}
		return out
	}
}

// spillDiagHandler builds a handler whose store fails Get with getErr and whose
// configured backend is storeBackend.
func spillDiagHandler(storeBackend string, getErr error) (*Handler, func(t *testing.T) []map[string]any) {
	h := &Handler{spillStore: &testSpillStore{backend: storeBackend, getErr: getErr}}
	return h, captureLogs(h)
}

// onlyRecord asserts exactly one log record was emitted and returns it.
func onlyRecord(t *testing.T, records []map[string]any) map[string]any {
	t.Helper()
	if len(records) != 1 {
		t.Fatalf("expected exactly 1 log record, got %d: %v", len(records), records)
	}
	return records[0]
}

func fieldString(t *testing.T, rec map[string]any, key string) string {
	t.Helper()
	v, ok := rec[key]
	if !ok {
		t.Fatalf("log record has no %q field: %v", key, rec)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("log field %q is not a string: %T", key, v)
	}
	return s
}

// A localfs ref that is not on this host is STRUCTURALLY unreachable: no retry,
// no credential fix, and no amount of waiting makes it readable here. The log
// has to say so, otherwise a multi-host localfs deployment is silently
// unreadable and reads as a transient fault.
func TestSpillFetch_LocalfsNotFound_NamesTheCrossHostCause(t *testing.T) {
	h, logs := spillDiagHandler("localfs", spillstore.ErrNotFound)
	ref := []byte(`{"backend":"localfs","key":"2026-07-27/evt-1-request.bin"}`)

	if _, err := h.rawSpillBody(context.Background(), ref); err == nil {
		t.Fatal("expected an error for a spill object missing from this host")
	} else {
		h.logSpillFetchFailure("normalize", "request", "evt-1", err)
	}

	rec := onlyRecord(t, logs(t))
	if got := fieldString(t, rec, "cause"); got != spillCauseNotFoundHostLocal {
		t.Fatalf("cause = %q, want %q — a body unreachable by deployment shape is being reported as an ordinary miss", got, spillCauseNotFoundHostLocal)
	}
	remedy := fieldString(t, rec, "remedy")
	if !strings.Contains(remedy, "only readable on the host that wrote it") {
		t.Fatalf("remedy does not state the host-local constraint, so the operator is not told why it is unreachable: %q", remedy)
	}
	if !strings.Contains(remedy, "shared backend") {
		t.Fatalf("remedy names no fix for a multi-host deployment: %q", remedy)
	}
}

// The mirror case. A shared backend's missing object is an ordinary miss (aged
// out, or an async upload lost before it landed). Blaming host locality here
// would send an operator to mount shared volumes for a body that was simply
// swept.
func TestSpillFetch_SharedBackendNotFound_IsNotBlamedOnHostLocality(t *testing.T) {
	h, logs := spillDiagHandler("s3", spillstore.ErrNotFound)
	ref := []byte(`{"backend":"s3","key":"spill/2026-07-27/evt-2-response.bin"}`)

	_, err := h.rawSpillBody(context.Background(), ref)
	if err == nil {
		t.Fatal("expected an error for an object absent from the bucket")
	}
	h.logSpillFetchFailure("view", "response", "evt-2", err)

	rec := onlyRecord(t, logs(t))
	if got := fieldString(t, rec, "cause"); got != spillCauseNotFound {
		t.Fatalf("cause = %q, want %q", got, spillCauseNotFound)
	}
	if remedy := fieldString(t, rec, "remedy"); strings.Contains(remedy, "host that wrote it") {
		t.Fatalf("a swept s3 object is being explained as a host-locality problem: %q", remedy)
	}
}

// A reachable-but-broken backend reported as "the body is gone" is the
// dangerous direction: an operator writes the record off as lost when the bytes
// are still there behind a credential or network fault.
func TestSpillFetch_TransportFailureIsNotReportedAsMissing(t *testing.T) {
	h, logs := spillDiagHandler("s3", errors.New("RequestCanceled: connect timeout"))
	ref := []byte(`{"backend":"s3","key":"spill/evt-3-request.bin"}`)

	_, err := h.rawSpillBody(context.Background(), ref)
	if err == nil {
		t.Fatal("expected an error when the backend is unreachable")
	}
	h.logSpillFetchFailure("view", "request", "evt-3", err)

	rec := onlyRecord(t, logs(t))
	if got := fieldString(t, rec, "cause"); got != spillCauseTransport {
		t.Fatalf("cause = %q, want %q — an unreachable backend must not be reported as a missing object", got, spillCauseTransport)
	}
}

// After a backend change, refs written under the old backend stay in the table.
// The diagnosis must name BOTH backends, because the fix (read them through the
// old backend, or accept the loss) depends on knowing which one wrote them.
func TestSpillFetch_BackendMismatchNamesBothBackends(t *testing.T) {
	h, logs := spillDiagHandler("s3", errors.New(`localfs.Get: ref backend "localfs" != "s3"`))
	ref := []byte(`{"backend":"localfs","key":"2026-07-01/evt-4-request.bin"}`)

	_, err := h.rawSpillBody(context.Background(), ref)
	if err == nil {
		t.Fatal("expected an error when the ref names a different backend")
	}
	h.logSpillFetchFailure("view", "request", "evt-4", err)

	rec := onlyRecord(t, logs(t))
	if got := fieldString(t, rec, "cause"); got != spillCauseBackendMismatch {
		t.Fatalf("cause = %q, want %q", got, spillCauseBackendMismatch)
	}
	remedy := fieldString(t, rec, "remedy")
	if !strings.Contains(remedy, `"localfs"`) || !strings.Contains(remedy, `"s3"`) {
		t.Fatalf("remedy names only one side of the mismatch: %q", remedy)
	}
	if got := fieldString(t, rec, "refBackend"); got != "localfs" {
		t.Fatalf("refBackend = %q, want localfs", got)
	}
	if got := fieldString(t, rec, "storeBackend"); got != "s3" {
		t.Fatalf("storeBackend = %q, want s3", got)
	}
}

// An integrity failure is a security event: the bytes at rest do not match what
// was recorded when the body was spilled. Folding it into "not found" would hide
// a tampered or cross-node-overwritten blob behind a routine-looking miss.
func TestSpillFetch_IntegrityFailureIsDistinctFromMissing(t *testing.T) {
	h := &Handler{spillStore: &testSpillStore{backend: "localfs", data: []byte(`{"forged":"content"}`)}}
	logs := captureLogs(h)
	// sha256 of some other content — the recorded digest cannot match.
	ref := []byte(`{"backend":"localfs","key":"k","sha256":"0000000000000000000000000000000000000000000000000000000000000000"}`)

	_, err := h.rawSpillBody(context.Background(), ref)
	if err == nil {
		t.Fatal("expected the integrity gate to refuse bytes that do not match the recorded digest")
	}
	h.logSpillFetchFailure("view", "request", "evt-5", err)

	rec := onlyRecord(t, logs(t))
	if got := fieldString(t, rec, "cause"); got != spillCauseIntegrity {
		t.Fatalf("cause = %q, want %q — a tampered blob must not be logged as an ordinary miss", got, spillCauseIntegrity)
	}
}

// A malformed spill_ref column is a data defect in the row itself, not a storage
// fault; pointing an operator at the backend would waste the investigation.
func TestSpillFetch_RefDecodeFailureIsAttributedToTheRow(t *testing.T) {
	h := &Handler{spillStore: &testSpillStore{backend: "localfs"}}
	logs := captureLogs(h)

	_, err := h.rawSpillBody(context.Background(), []byte("not-json"))
	if err == nil {
		t.Fatal("expected an error for an undecodable spill_ref")
	}
	h.logSpillFetchFailure("normalize", "request", "evt-6", err)

	rec := onlyRecord(t, logs(t))
	if got := fieldString(t, rec, "cause"); got != spillCauseRefDecode {
		t.Fatalf("cause = %q, want %q", got, spillCauseRefDecode)
	}
	if remedy := fieldString(t, rec, "remedy"); !strings.Contains(remedy, "spill_ref") {
		t.Fatalf("remedy does not point at the row's own column: %q", remedy)
	}
}

// The object resolved and then the stream broke. That is a live backend with a
// transfer fault — distinct from both "absent" and "unreachable", because the
// object is known to exist.
func TestSpillFetch_ReadFailureIsDistinctFromNotFound(t *testing.T) {
	h := &Handler{spillStore: &testSpillStore{backend: "s3", readErr: errors.New("connection reset mid-read")}}
	logs := captureLogs(h)
	ref := []byte(`{"backend":"s3","key":"k"}`)

	_, err := h.rawSpillBody(context.Background(), ref)
	if err == nil {
		t.Fatal("expected an error when the object stream breaks mid-read")
	}
	h.logSpillFetchFailure("view", "response", "evt-7", err)

	rec := onlyRecord(t, logs(t))
	if got := fieldString(t, rec, "cause"); got != spillCauseRead {
		t.Fatalf("cause = %q, want %q", got, spillCauseRead)
	}
}

// Four read sites share one log line, so without stage+direction an operator
// cannot tell WHICH body failed — the drawer render or the normalize recompute,
// request side or response side.
//
// This drives the REAL call sites. An earlier version passed its own literals to
// logSpillFetchFailure, so every one of the four production sites could be
// mislabelled and the test stayed green — it asserted the log helper's plumbing,
// not the thing its comment claimed.
func TestFillSpilledBodies_LabelsEachRealCallSite(t *testing.T) {
	h, logs := spillDiagHandler("localfs", spillstore.ErrNotFound)
	ref := []byte(`{"backend":"localfs","key":"k"}`)

	h.fillSpilledBodies(context.Background(), "evt-8", &trafficstore.NormalizeInput{
		RequestSpillRef:  ref,
		ResponseSpillRef: ref,
	})

	records := logs(t)
	if len(records) != 2 {
		t.Fatalf("expected one record per direction, got %d", len(records))
	}
	for i, want := range []struct{ stage, direction string }{
		{"normalize", "request"},
		{"normalize", "response"},
	} {
		if got := fieldString(t, records[i], "stage"); got != want.stage {
			t.Fatalf("record %d stage = %q, want %q — the normalize reader is mislabelled", i, got, want.stage)
		}
		if got := fieldString(t, records[i], "direction"); got != want.direction {
			t.Fatalf("record %d direction = %q, want %q — an operator cannot tell which body failed", i, got, want.direction)
		}
		if got := fieldString(t, records[i], "trafficEventId"); got != "evt-8" {
			t.Fatalf("record %d trafficEventId = %q, want evt-8", i, got)
		}
	}
}

// The success half of the same path: a body that fetches cleanly is folded onto
// the input AND produces no failure line. Driven through fillSpilledBodies,
// because that is where the success-vs-failure branch actually lives — asserting
// it against rawSpillBody alone would be unfalsifiable, since rawSpillBody does
// not log at all.
func TestFillSpilledBodies_SuccessFoldsBodyAndLogsNothing(t *testing.T) {
	h := &Handler{spillStore: &testSpillStore{backend: "localfs", data: []byte(`{"ok":true}`)}}
	logs := captureLogs(h)
	in := &trafficstore.NormalizeInput{RequestSpillRef: []byte(`{"backend":"localfs","key":"k"}`)}

	h.fillSpilledBodies(context.Background(), "evt-ok", in)

	if string(in.RequestBody) != `{"ok":true}` {
		t.Fatalf("RequestBody = %q, want the spilled bytes folded in verbatim", in.RequestBody)
	}
	if records := logs(t); len(records) != 0 {
		t.Fatalf("a successful fetch emitted %d failure records: %v", len(records), records)
	}
}

// A non-*spillFetchError still logs. The diagnosis enriches the line; it must
// never be the thing that decides whether the operator is told at all.
func TestSpillFetchLog_UnclassifiedErrorStillLogs(t *testing.T) {
	h := &Handler{spillStore: &testSpillStore{backend: "localfs"}}
	logs := captureLogs(h)

	h.logSpillFetchFailure("view", "request", "evt-9", errors.New("some other failure"))

	rec := onlyRecord(t, logs(t))
	if _, ok := rec["cause"]; ok {
		t.Fatal("an unclassified error must not carry a fabricated cause label")
	}
	if got := fieldString(t, rec, "error"); got != "some other failure" {
		t.Fatalf("error field = %q, want the underlying message", got)
	}
}

// A handler with no logger must not panic on the failure path — the read path
// degrades to an empty body either way.
func TestSpillFetchLog_NilLoggerIsSafe(t *testing.T) {
	h := &Handler{spillStore: &testSpillStore{backend: "localfs"}}
	h.logSpillFetchFailure("view", "request", "evt-10", errors.New("boom"))
}

// storeBackendName reports "" with no store wired, so the diagnosis never
// dereferences a nil interface while classifying.
func TestStoreBackendName_NoStoreWired(t *testing.T) {
	h := &Handler{}
	if got := h.storeBackendName(); got != "" {
		t.Fatalf("storeBackendName() = %q, want empty when no spill store is configured", got)
	}
}

// The diagnosis must ENRICH the underlying error, never mask it. spillstore
// documents ErrNotFound as the "already gone" signal callers key on; a wrapper
// that swallowed it would silently turn a recognised condition into an opaque
// failure at every future call site.
func TestSpillFetchError_PreservesTheUnderlyingSentinel(t *testing.T) {
	h, _ := spillDiagHandler("localfs", spillstore.ErrNotFound)

	_, err := h.rawSpillBody(context.Background(), []byte(`{"backend":"localfs","key":"k"}`))
	if !errors.Is(err, spillstore.ErrNotFound) {
		t.Fatal("the diagnosis wrapper masked spillstore.ErrNotFound; callers keying on the documented \"already gone\" sentinel would stop recognising it")
	}
}

// resolveSpillBody (the drawer path) must produce the same diagnosis as the
// normalize path — they share one fetch helper precisely so the two cannot drift.
func TestResolveSpillBody_SharesTheSameDiagnosis(t *testing.T) {
	h, logs := spillDiagHandler("localfs", spillstore.ErrNotFound)
	ref := []byte(`{"backend":"localfs","key":"k"}`)

	_, err := h.resolveSpillBody(context.Background(), ref)
	if err == nil {
		t.Fatal("expected an error from the drawer read path")
	}
	h.logSpillFetchFailure("view", "request", "evt-11", err)

	rec := onlyRecord(t, logs(t))
	if got := fieldString(t, rec, "cause"); got != spillCauseNotFoundHostLocal {
		t.Fatalf("drawer path cause = %q, want %q — the two read paths have drifted", got, spillCauseNotFoundHostLocal)
	}
}

// An unsealable object is an INTEGRITY event, not a transport one, and the two
// remedies point in opposite directions: "check connectivity and credentials"
// invites a retry, while "the stored bytes are not the bytes that were written"
// on a compliance product is the one an operator must not retry away.
//
// The path is real rather than hypothetical — localfs.Get GCM-opens an encrypted
// object and returns spillstore.ErrIntegrity when the seal will not open — and
// before this it fell through to the transport arm, because that arm was
// everything-that-is-not-ErrNotFound. The SHA-256 gate cannot cover it either:
// that fires on bytes that decode but hash wrong, and a failed seal yields no
// bytes to hash.
func TestSpillFetch_UnsealableObjectIsIntegrityNotTransport(t *testing.T) {
	h, logs := spillDiagHandler("localfs", fmt.Errorf("localfs.Get: decrypt: %w: cipher: message authentication failed",
		spillstore.ErrIntegrity))
	ref := []byte(`{"backend":"localfs","key":"spill/evt-9-response.bin"}`)

	_, err := h.rawSpillBody(context.Background(), ref)
	if err == nil {
		t.Fatal("expected an error when the object cannot be unsealed")
	}
	h.logSpillFetchFailure("view", "response", "evt-9", err)

	rec := onlyRecord(t, logs(t))
	if got := fieldString(t, rec, "cause"); got != spillCauseIntegrity {
		t.Fatalf("cause = %q, want %q — an object that was found and read but will not unseal is not a "+
			"connectivity problem, and reporting it as one invites the retry that overwrites the evidence",
			got, spillCauseIntegrity)
	}
	remedy := fieldString(t, rec, "remedy")
	if !strings.Contains(remedy, "NOT a connectivity problem") {
		t.Errorf("remedy = %q; it must tell the operator explicitly not to treat this as transport, "+
			"because that is the arm this case used to land in", remedy)
	}
}

// The classifier can only be right if the backend actually raises the sentinel.
// Testing the classifier alone would pass against a localfs that returned a bare
// error, leaving production on the transport arm — the D8' trap: reaching the
// changed lines from a test says nothing about whether production reaches them.
func TestLocalfsGet_UnsealableObjectCarriesTheIntegritySentinel(t *testing.T) {
	root := t.TempDir()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	st, err := localfs.New(localfs.Options{Root: root, EncryptionKey: key})
	if err != nil {
		t.Skipf("localfs encryption unavailable in this build: %v", err)
	}
	ctx := context.Background()
	body := []byte("the captured body")
	ref, err := st.Put(ctx, bytes.NewReader(body), int64(len(body)), spillstore.PutOptions{
		EventID: "evt-tamper", Direction: "response",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Tamper: flip a byte of the sealed file on disk, which is exactly what this
	// classification exists to describe.
	path := filepath.Join(root, ref.Key)
	sealed, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read sealed object: %v", rerr)
	}
	sealed[len(sealed)-1] ^= 0xFF
	if werr := os.WriteFile(path, sealed, 0o600); werr != nil {
		t.Fatalf("write tampered object: %v", werr)
	}

	_, gerr := st.Get(ctx, ref)
	if gerr == nil {
		t.Fatal("Get returned no error for a tampered sealed object: the GCM tag did not reject it")
	}
	if !errors.Is(gerr, spillstore.ErrIntegrity) {
		t.Fatalf("Get error %v does not wrap spillstore.ErrIntegrity, so the Control Plane classifier "+
			"falls through to the transport arm and tells an operator to check connectivity", gerr)
	}
	if errors.Is(gerr, spillstore.ErrNotFound) {
		t.Fatal("a tampered object must not read as ErrNotFound: the record would be written off as gone")
	}
}
