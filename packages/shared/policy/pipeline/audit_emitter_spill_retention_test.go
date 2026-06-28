package pipeline

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// memSpill stores objects in memory and returns real refs so EmitBody
// takes the spill branch.
type memSpill struct{ objects map[string][]byte }

func (m *memSpill) Put(_ context.Context, content io.Reader, size int64, opts spillstore.PutOptions) (audit.SpillRef, error) {
	if m.objects == nil {
		m.objects = map[string][]byte{}
	}
	b, err := io.ReadAll(content)
	if err != nil {
		return audit.SpillRef{}, err
	}
	key := opts.EventID + "/" + opts.Direction
	m.objects[key] = b
	return audit.SpillRef{Backend: "mem", Key: key, Size: size}, nil
}
func (m *memSpill) Get(_ context.Context, ref audit.SpillRef) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m.objects[ref.Key])), nil
}
func (m *memSpill) Delete(context.Context, audit.SpillRef) error  { return nil }
func (m *memSpill) Backend() string                               { return "mem" }
func (m *memSpill) Sweep(context.Context, time.Time) (int, error) { return 0, nil }
func (m *memSpill) Stat(context.Context) (spillstore.Stats, error) {
	return spillstore.Stats{Backend: "mem"}, nil
}

// TestBuildEvent_SpilledBodiesRetainInMemoryBytes pins the pre-spill
// normalize contract: a body that spills still carries its bytes on the
// in-memory Body container, so the audit writer's normalize pass sees
// the content without a spill-store round-trip; the wire form (custom
// MarshalJSON) keeps only the spill ref.
func TestBuildEvent_SpilledBodiesRetainInMemoryBytes(t *testing.T) {
	w := &captureWriter{}
	e := NewAuditEmitter(w, testEmitterLogger()).
		WithSpillStore(&memSpill{}).
		WithPreSpillNormalize().
		WithPayloadCaptureStore(payloadcapture.NewStore(payloadcapture.Config{
			MaxInlineBodyBytes: 8, // force the spill branch for both bodies
		}))

	reqBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	respBody := []byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	e.EmitDual(
		&core.HookInput{IngressType: "COMPLIANCE_PROXY", TargetHost: "api.openai.com", Path: "/v1/chat/completions", Method: "POST"},
		AuditInfo{TransactionID: "txn-spill"},
		&core.CompliancePipelineResult{Decision: core.Approve, Action: core.ActionApprove}, nil,
		"BUMP_SUCCESS", 200, 5, reqBody, respBody, traffic.UsageMeta{},
	)

	if got := w.count(); got != 1 {
		t.Fatalf("expected 1 event, got %d", got)
	}
	ev := w.events[0]
	if ev.RequestBody.Kind != audit.BodySpill || ev.ResponseBody.Kind != audit.BodySpill {
		t.Fatalf("bodies should have spilled (kinds %s/%s)", ev.RequestBody.Kind, ev.ResponseBody.Kind)
	}
	if string(ev.RequestBody.InlineBytes) != string(reqBody) {
		t.Errorf("spilled request container lost its in-memory bytes: %q", ev.RequestBody.InlineBytes)
	}
	if string(ev.ResponseBody.InlineBytes) != string(respBody) {
		t.Errorf("spilled response container lost its in-memory bytes: %q", ev.ResponseBody.InlineBytes)
	}
	// The wire form must NOT leak the bytes for a spill container.
	wire, err := ev.RequestBody.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(wire), "hello") {
		t.Errorf("spill container wire form must carry the ref only, got %s", wire)
	}
}

// Without WithPreSpillNormalize (the agent path: writer normalizes
// inline before emit), a spilled container is NOT loaded with in-memory
// bytes — retention is pure cost there. The hub backfill heals it.
func TestBuildEvent_NoPreSpillNormalize_LeavesSpillRefOnly(t *testing.T) {
	w := &captureWriter{}
	e := NewAuditEmitter(w, testEmitterLogger()).
		WithSpillStore(&memSpill{}).
		WithPayloadCaptureStore(payloadcapture.NewStore(payloadcapture.Config{MaxInlineBodyBytes: 8}))

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	e.EmitDual(
		&core.HookInput{IngressType: "AGENT", TargetHost: "api.openai.com", Path: "/v1/chat/completions", Method: "POST"},
		AuditInfo{TransactionID: "txn-agent"},
		&core.CompliancePipelineResult{Decision: core.Approve, Action: core.ActionApprove}, nil,
		"BUMP_SUCCESS", 200, 5, body, nil, traffic.UsageMeta{},
	)
	ev := w.events[0]
	if ev.RequestBody.Kind != audit.BodySpill {
		t.Fatalf("body should have spilled, got %s", ev.RequestBody.Kind)
	}
	if ev.RequestBody.InlineBytes != nil {
		t.Errorf("without opt-in, a spilled container must stay ref-only, got %q", ev.RequestBody.InlineBytes)
	}
}

// A spilled body larger than spillRetainCap stays ref-only even with the
// opt-in: the cap bounds queued memory; the hub backfill spill-fetch
// (off the hot path) heals the oversize row.
func TestBuildEvent_OversizeSpillBodyStaysRefOnly(t *testing.T) {
	w := &captureWriter{}
	e := NewAuditEmitter(w, testEmitterLogger()).
		WithSpillStore(&memSpill{}).
		WithPreSpillNormalize().
		WithPayloadCaptureStore(payloadcapture.NewStore(payloadcapture.Config{MaxInlineBodyBytes: 8}))

	big := make([]byte, spillRetainCap+1)
	for i := range big {
		big[i] = 'a'
	}
	e.EmitDual(
		&core.HookInput{IngressType: "COMPLIANCE_PROXY", TargetHost: "api.openai.com", Path: "/v1/chat/completions", Method: "POST"},
		AuditInfo{TransactionID: "txn-big"},
		&core.CompliancePipelineResult{Decision: core.Approve, Action: core.ActionApprove}, nil,
		"BUMP_SUCCESS", 200, 5, big, nil, traffic.UsageMeta{},
	)
	ev := w.events[0]
	if ev.RequestBody.Kind != audit.BodySpill {
		t.Fatalf("body should have spilled, got %s", ev.RequestBody.Kind)
	}
	if ev.RequestBody.InlineBytes != nil {
		t.Errorf("over-cap spilled body must stay ref-only, got %d bytes retained", len(ev.RequestBody.InlineBytes))
	}
}
