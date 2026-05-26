package loaders

import (
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
)

// The DB-bound LoadInterceptionDomainsFull is a thin shell that delegates
// to decodeInterceptionDomainRows + attachInterceptionPaths; the
// interesting decoding logic (enum upper-casing, path JSON decode,
// orphan-path drop, malformed-pattern hard error) is tested here
// without a live database.

func TestDecodeInterceptionDomainRows_EmptyInputYieldsEmpty(t *testing.T) {
	out, byID := decodeInterceptionDomainRows(nil)
	if len(out) != 0 {
		t.Errorf("nil input must yield empty out, got len %d", len(out))
	}
	if len(byID) != 0 {
		t.Errorf("nil input must yield empty index, got %v", byID)
	}
}

func TestDecodeInterceptionDomainRows_EnumsUpperCased(t *testing.T) {
	// The DB returns enum::text values already upper-cased, but the
	// loader applies ToUpper defensively so a future driver / SELECT
	// shape (e.g. directly bound enum) does not silently produce a
	// mixed-case literal that the engine's switch statements would not
	// match.
	updatedAt := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	rows := []InterceptionDomainRow{
		{
			ID: "d1", Name: "openai", HostPattern: "api.openai.com",
			HostMatch: "exact", AdapterID: "openai-v1",
			Zone: "public", DefaultPathAction: "process", OnAdapterError: "fail_open",
			Enabled: true, Priority: 10, UpdatedAt: updatedAt,
		},
	}
	out, byID := decodeInterceptionDomainRows(rows)
	if len(out) != 1 {
		t.Fatalf("len: %d", len(out))
	}
	d := out[0]
	if d.HostMatchType != domain.HostMatchExact {
		t.Errorf("HostMatchType not upper-cased: %q", d.HostMatchType)
	}
	if d.NetworkZone != domain.ZonePublic {
		t.Errorf("NetworkZone not upper-cased: %q", d.NetworkZone)
	}
	if d.DefaultPathAction != domain.PathActionProcess {
		t.Errorf("DefaultPathAction not upper-cased: %q", d.DefaultPathAction)
	}
	if d.OnAdapterError != domain.AdapterErrorFailOpen {
		t.Errorf("OnAdapterError not upper-cased: %q", d.OnAdapterError)
	}
	if !d.UpdatedAt.Equal(updatedAt) {
		t.Errorf("UpdatedAt not threaded: %v", d.UpdatedAt)
	}
	if byID["d1"] != 0 {
		t.Errorf("id→index map wrong: %v", byID)
	}
	if d.Paths != nil {
		t.Errorf("Paths must start nil before attach; got %v", d.Paths)
	}
}

func TestDecodeInterceptionDomainRows_PointerOverridesThreaded(t *testing.T) {
	// Streaming + capture columns are *string / *int / *bool because
	// NULL means "inherit global default". The loader must pass the
	// pointer (not a copy) through so the engine's resolve step can
	// distinguish "unset" from "explicitly zero".
	mode := "buffer_full_block"
	chunkBytes := 8192
	hookTimeoutMs := 500
	maxBufferBytes := 65536
	failBehavior := "fail_closed"
	captureReq := true
	captureResp := false
	rawSpill := true
	rows := []InterceptionDomainRow{
		{
			ID: "d1", Name: "n",
			HostPattern: "h", HostMatch: "EXACT",
			Zone: "INTERNAL", DefaultPathAction: "PROCESS", OnAdapterError: "FAIL_OPEN",
			Enabled: true, Priority: 1,
			StreamingMode:           &mode,
			StreamingChunkBytes:     &chunkBytes,
			StreamingHookTimeoutMs:  &hookTimeoutMs,
			StreamingMaxBufferBytes: &maxBufferBytes,
			StreamingFailBehavior:   &failBehavior,
			CaptureRequestBody:      &captureReq,
			CaptureResponseBody:     &captureResp,
			RawBodySpillEnabled:     &rawSpill,
		},
	}
	out, _ := decodeInterceptionDomainRows(rows)
	d := out[0]
	if d.StreamingMode == nil || *d.StreamingMode != mode {
		t.Errorf("StreamingMode pointer dropped: %v", d.StreamingMode)
	}
	if d.StreamingChunkBytes == nil || *d.StreamingChunkBytes != chunkBytes {
		t.Errorf("StreamingChunkBytes pointer dropped: %v", d.StreamingChunkBytes)
	}
	if d.StreamingHookTimeoutMs == nil || *d.StreamingHookTimeoutMs != hookTimeoutMs {
		t.Errorf("StreamingHookTimeoutMs pointer dropped: %v", d.StreamingHookTimeoutMs)
	}
	if d.StreamingMaxBufferBytes == nil || *d.StreamingMaxBufferBytes != maxBufferBytes {
		t.Errorf("StreamingMaxBufferBytes pointer dropped: %v", d.StreamingMaxBufferBytes)
	}
	if d.StreamingFailBehavior == nil || *d.StreamingFailBehavior != failBehavior {
		t.Errorf("StreamingFailBehavior pointer dropped: %v", d.StreamingFailBehavior)
	}
	if d.CaptureRequestBody == nil || *d.CaptureRequestBody != captureReq {
		t.Errorf("CaptureRequestBody pointer dropped: %v", d.CaptureRequestBody)
	}
	if d.CaptureResponseBody == nil || *d.CaptureResponseBody != captureResp {
		t.Errorf("CaptureResponseBody pointer dropped: %v", d.CaptureResponseBody)
	}
	if d.RawBodySpillEnabled == nil || *d.RawBodySpillEnabled != rawSpill {
		t.Errorf("RawBodySpillEnabled pointer dropped: %v", d.RawBodySpillEnabled)
	}
}

func TestDecodeInterceptionDomainRows_NilPointersStayNil(t *testing.T) {
	// "Inherit global default" semantics — nil pointers must NOT be
	// auto-filled by the decode helper; the resolve layer downstream
	// is the only thing that translates nil → global default.
	rows := []InterceptionDomainRow{
		{
			ID: "d1", HostMatch: "EXACT",
			Zone: "PUBLIC", DefaultPathAction: "PROCESS", OnAdapterError: "FAIL_OPEN",
			Enabled: true,
			// All pointer fields left nil.
		},
	}
	out, _ := decodeInterceptionDomainRows(rows)
	d := out[0]
	if d.StreamingMode != nil || d.StreamingChunkBytes != nil ||
		d.StreamingHookTimeoutMs != nil || d.StreamingMaxBufferBytes != nil ||
		d.StreamingFailBehavior != nil || d.CaptureRequestBody != nil ||
		d.CaptureResponseBody != nil || d.RawBodySpillEnabled != nil {
		t.Errorf("nil pointer fields must stay nil for inherit-default semantics: %+v", d)
	}
}

func TestDecodeInterceptionDomainRows_PreservesOrderAndBuildsIndex(t *testing.T) {
	// Multiple rows: the SQL caller pre-orders by priority DESC,
	// created_at ASC. The pure decoder must preserve that order so the
	// engine's first-match-wins logic stays priority-aware.
	rows := []InterceptionDomainRow{
		{ID: "high-priority", HostMatch: "EXACT", Zone: "PUBLIC", DefaultPathAction: "PROCESS", OnAdapterError: "FAIL_OPEN"},
		{ID: "mid-priority", HostMatch: "EXACT", Zone: "PUBLIC", DefaultPathAction: "PROCESS", OnAdapterError: "FAIL_OPEN"},
		{ID: "low-priority", HostMatch: "EXACT", Zone: "PUBLIC", DefaultPathAction: "PROCESS", OnAdapterError: "FAIL_OPEN"},
	}
	out, byID := decodeInterceptionDomainRows(rows)
	for i, id := range []string{"high-priority", "mid-priority", "low-priority"} {
		if out[i].ID != id {
			t.Errorf("position %d: got %q, want %q", i, out[i].ID, id)
		}
		if byID[id] != i {
			t.Errorf("index for %q: got %d, want %d", id, byID[id], i)
		}
	}
}

// attachInterceptionPaths tests cover (a) happy path, (b) orphan rows
// silently dropped, (c) malformed JSON aborts with attribution.

func TestAttachInterceptionPaths_EmptyPathsLeavesDomainsUntouched(t *testing.T) {
	domains, byID := decodeInterceptionDomainRows([]InterceptionDomainRow{
		{ID: "d1", HostMatch: "EXACT", Zone: "PUBLIC", DefaultPathAction: "PROCESS", OnAdapterError: "FAIL_OPEN"},
	})
	out, err := attachInterceptionPaths(domains, byID, nil)
	if err != nil {
		t.Fatalf("nil paths must NOT error: %v", err)
	}
	if len(out[0].Paths) != 0 {
		t.Errorf("nil input must leave Paths empty/nil; got %v", out[0].Paths)
	}
}

func TestAttachInterceptionPaths_PatternsDecodedAndStamped(t *testing.T) {
	// Two paths attached to one domain. Pattern JSON arrives as a
	// to_jsonb-encoded text[]. The decoder must split it into a Go
	// []string and stamp the upper-cased match-type + action.
	domains, byID := decodeInterceptionDomainRows([]InterceptionDomainRow{
		{ID: "d1", HostMatch: "EXACT", Zone: "PUBLIC", DefaultPathAction: "PROCESS", OnAdapterError: "FAIL_OPEN"},
	})
	pathRows := []InterceptionPathRow{
		{ID: "p1", DomainID: "d1", PatternsJSON: `["/v1/chat/*"]`, MatchType: "prefix", Action: "process"},
		{ID: "p2", DomainID: "d1", PatternsJSON: `["/v1/embeddings","/v1/audio/*"]`, MatchType: "exact", Action: "block"},
	}
	out, err := attachInterceptionPaths(domains, byID, pathRows)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out[0].Paths) != 2 {
		t.Fatalf("path count: got %d, want 2", len(out[0].Paths))
	}
	if out[0].Paths[0].MatchType != domain.PathMatchPrefix {
		t.Errorf("path match-type not upper-cased: %q", out[0].Paths[0].MatchType)
	}
	if out[0].Paths[0].Action != domain.PathActionProcess {
		t.Errorf("path action not upper-cased: %q", out[0].Paths[0].Action)
	}
	if len(out[0].Paths[1].PathPattern) != 2 {
		t.Errorf("multi-pattern not decoded: %v", out[0].Paths[1].PathPattern)
	}
	if out[0].Paths[1].PathPattern[0] != "/v1/embeddings" || out[0].Paths[1].PathPattern[1] != "/v1/audio/*" {
		t.Errorf("multi-pattern values: %v", out[0].Paths[1].PathPattern)
	}
	if out[0].Paths[1].Action != domain.PathActionBlock {
		t.Errorf("action upper-cased: %q", out[0].Paths[1].Action)
	}
}

func TestAttachInterceptionPaths_OrphanPathSilentlyDropped(t *testing.T) {
	// A path whose domain_id is not in the loaded set must be silently
	// dropped — this happens when a path references a disabled
	// (therefore not-loaded) domain. The contract is: drop, do NOT
	// error, do NOT modify other domains' paths.
	domains, byID := decodeInterceptionDomainRows([]InterceptionDomainRow{
		{ID: "d1", HostMatch: "EXACT", Zone: "PUBLIC", DefaultPathAction: "PROCESS", OnAdapterError: "FAIL_OPEN"},
	})
	pathRows := []InterceptionPathRow{
		{ID: "p1", DomainID: "ghost", PatternsJSON: `["/leak"]`, MatchType: "PREFIX", Action: "PROCESS"},
		{ID: "p2", DomainID: "d1", PatternsJSON: `["/ok"]`, MatchType: "PREFIX", Action: "PROCESS"},
	}
	out, err := attachInterceptionPaths(domains, byID, pathRows)
	if err != nil {
		t.Fatalf("orphan path must NOT error: %v", err)
	}
	if len(out[0].Paths) != 1 {
		t.Errorf("only the matching path should attach; got %v", out[0].Paths)
	}
	if out[0].Paths[0].ID != "p2" {
		t.Errorf("wrong path attached: %v", out[0].Paths[0])
	}
}

func TestAttachInterceptionPaths_MalformedPatternsJSONAbortsWithAttribution(t *testing.T) {
	// A corrupt path_pattern JSON row must NOT be silently swallowed —
	// the resulting domain would have an empty PathPattern slice and
	// the engine would default to DefaultPathAction for that path,
	// which is a silently-wrong policy. Aborting the whole load is the
	// load-bearing contract.
	domains, byID := decodeInterceptionDomainRows([]InterceptionDomainRow{
		{ID: "d1", HostMatch: "EXACT", Zone: "PUBLIC", DefaultPathAction: "PROCESS", OnAdapterError: "FAIL_OPEN"},
	})
	pathRows := []InterceptionPathRow{
		{ID: "p1", DomainID: "d1", PatternsJSON: `not json`, MatchType: "PREFIX", Action: "PROCESS"},
	}
	_, err := attachInterceptionPaths(domains, byID, pathRows)
	if err == nil {
		t.Fatal("malformed path_pattern JSON must surface an error")
	}
	if !strings.Contains(err.Error(), "decode path_pattern") {
		t.Errorf("err must carry attribution prefix; got: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "not json") {
		t.Errorf("err must echo the malformed input so operators can debug: %q", err.Error())
	}
}
