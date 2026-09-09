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

// TestBuildEvent_SpilledBodyStaysRefOnly asserts, unconditionally and at any
// size, that a spilled body never carries its bytes on the container. Both the
// small and the over-2-MiB case are covered: retaining a large body in memory
// is the expensive way to break this, and the audit queue holds up to ~1000
// events until flush.
//
// It also carries the wire form: Body.MarshalJSON must emit the ref alone for a
// spill container. That is a payload-leak path on a DLP product, and nothing
// else covers it.
func TestBuildEvent_SpilledBodyStaysRefOnly(t *testing.T) {
	const secret = "hello"
	small := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"` + secret + `"}]}`)
	large := bytes.Repeat([]byte("a"), (2<<20)+1) // comfortably past any in-memory cap

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"small spilled body", small},
		{"body larger than the deleted 2 MiB retain cap", large},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &captureWriter{}
			e := NewAuditEmitter(w, testEmitterLogger()).
				WithSpillStore(&memSpill{}).
				WithPayloadCaptureStore(payloadcapture.NewStore(payloadcapture.Config{
					MaxInlineBodyBytes: 8, // force the spill branch
				}))

			e.EmitDual(
				&core.HookInput{IngressType: "COMPLIANCE_PROXY", TargetHost: "api.openai.com", Path: "/v1/chat/completions", Method: "POST"},
				AuditInfo{TransactionID: "txn-spill"},
				&core.CompliancePipelineResult{Decision: core.Approve, Action: core.ActionApprove}, nil,
				"BUMP_SUCCESS", 200, 5, tc.body, nil, traffic.UsageMeta{},
			)

			if got := w.count(); got != 1 {
				t.Fatalf("expected 1 event, got %d", got)
			}
			ev := w.events[0]
			if ev.RequestBody.Kind != audit.BodySpill {
				t.Fatalf("body should have spilled, got kind %s", ev.RequestBody.Kind)
			}
			if ev.RequestBody.InlineBytes != nil {
				t.Errorf("a spilled container must stay ref-only, got %d bytes retained. The audit "+
					"queue holds up to ~1000 events until flush, so retaining bodies here pins "+
					"memory proportional to the queue depth for no consumer.",
					len(ev.RequestBody.InlineBytes))
			}
		})
	}

	// The wire form must never leak the payload for a spill container, whatever the in-memory
	// container holds. Folded in from the deleted test rather than dropped with it.
	w := &captureWriter{}
	e := NewAuditEmitter(w, testEmitterLogger()).
		WithSpillStore(&memSpill{}).
		WithPayloadCaptureStore(payloadcapture.NewStore(payloadcapture.Config{MaxInlineBodyBytes: 8}))
	e.EmitDual(
		&core.HookInput{IngressType: "COMPLIANCE_PROXY", TargetHost: "api.openai.com", Path: "/v1/chat/completions", Method: "POST"},
		AuditInfo{TransactionID: "txn-wire"},
		&core.CompliancePipelineResult{Decision: core.Approve, Action: core.ActionApprove}, nil,
		"BUMP_SUCCESS", 200, 5, small, nil, traffic.UsageMeta{},
	)
	wire, err := w.events[0].RequestBody.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(wire), secret) {
		t.Errorf("the spill container's wire form must carry the ref only, got %s", wire)
	}
}
