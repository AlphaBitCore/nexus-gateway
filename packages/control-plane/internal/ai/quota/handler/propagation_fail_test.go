package quota

// Quota policy/override writes must fail loud (HTTP 502)
// when the Category B invalidation push to Hub fails, so the data plane does
// not keep enforcing a stale spend cap while the UI reports success. Each test
// asserts the CP DB write committed (truth preserved), the response is 502 with
// the propagation_error envelope, and NO success audit row was enqueued.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/virtualkeys/vkstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/orgstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
)

// countingProducer counts audit enqueues so a test can assert that no success
// audit row was written when the Hub push failed.
type countingProducer struct {
	mu    sync.Mutex
	count int
}

func (p *countingProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (p *countingProducer) Enqueue(_ context.Context, _ string, _ []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.count++
	return nil
}
func (p *countingProducer) Close() error { return nil }

func (p *countingProducer) seen() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count
}

// newHandlerWithAuditCount wires a quota Handler whose audit writer routes
// through a countingProducer so the test can assert audit suppression.
func newHandlerWithAuditCount(db quotaDB, hub HubAPI) (*Handler, *countingProducer) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	prod := &countingProducer{}
	return &Handler{
		quota:   db,
		metrics: &fakeMetricsDB{},
		// Found-by-default doubles so the referential checks pass on the
		// committed-write path these tests exercise.
		users:  &fakeUsersDB{user: &userstore.NexusUserSafe{DisplayName: "u"}},
		orgs:   &fakeOrgsDB{org: &orgstore.Organization{Name: "o"}, project: &orgstore.Project{Name: "p"}},
		vks:    &fakeVKsDB{vk: &vkstore.VirtualKey{Name: "vk"}},
		hub:    hub,
		audit:  audit.NewWriter(prod, "audit", logger),
		logger: logger,
	}, prod
}

func TestCreateQuotaPolicy_HubFailure502(t *testing.T) {
	db := newFakeQuotaDB()
	spy := &fakeHubAPI{invalidateErr: errors.New("hub unreachable")}
	h, prod := newHandlerWithAuditCount(db, spy)

	c, rec := echoCtx(http.MethodPost, "/quota-policies", validCreatePolicyBody())
	if err := h.CreateQuotaPolicy(c); err != nil {
		t.Fatalf("CreateQuotaPolicy: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502; body=%s", rec.Code, rec.Body.String())
	}
	assertQuotaPropagationEnvelope(t, rec)
	if len(db.policies) != 1 {
		t.Errorf("policy not persisted on push failure: have %d; want 1 (DB is source of truth)", len(db.policies))
	}
	if len(spy.seen()) != 1 {
		t.Errorf("invalidate attempts=%d; want 1", len(spy.seen()))
	}
	if prod.seen() != 0 {
		t.Errorf("audit enqueues=%d; want 0 (must not log success on push failure)", prod.seen())
	}
}

func TestUpdateQuotaPolicy_HubFailure502(t *testing.T) {
	db := newFakeQuotaDB()
	pol := samplePolicy()
	db.policies[pol.ID] = &pol
	spy := &fakeHubAPI{invalidateErr: errors.New("hub down")}
	h, prod := newHandlerWithAuditCount(db, spy)

	c, rec := echoCtx(http.MethodPut, "/quota-policies/"+pol.ID, `{"name":"updated"}`)
	c.SetParamNames("id")
	c.SetParamValues(pol.ID)
	if err := h.UpdateQuotaPolicy(c); err != nil {
		t.Fatalf("UpdateQuotaPolicy: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502; body=%s", rec.Code, rec.Body.String())
	}
	assertQuotaPropagationEnvelope(t, rec)
	if prod.seen() != 0 {
		t.Errorf("audit enqueues=%d; want 0", prod.seen())
	}
}

func TestDeleteQuotaPolicy_HubFailure502(t *testing.T) {
	db := newFakeQuotaDB()
	pol := samplePolicy()
	db.policies[pol.ID] = &pol
	spy := &fakeHubAPI{invalidateErr: errors.New("hub down")}
	h, prod := newHandlerWithAuditCount(db, spy)

	c, rec := echoCtx(http.MethodDelete, "/quota-policies/"+pol.ID, "")
	c.SetParamNames("id")
	c.SetParamValues(pol.ID)
	if err := h.DeleteQuotaPolicy(c); err != nil {
		t.Fatalf("DeleteQuotaPolicy: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502; body=%s", rec.Code, rec.Body.String())
	}
	assertQuotaPropagationEnvelope(t, rec)
	if _, stillThere := db.policies[pol.ID]; stillThere {
		t.Error("policy should have been deleted before the push (DB is source of truth)")
	}
	if prod.seen() != 0 {
		t.Errorf("audit enqueues=%d; want 0", prod.seen())
	}
}

func TestCreateQuotaOverride_HubFailure502(t *testing.T) {
	db := newFakeQuotaDB()
	spy := &fakeHubAPI{invalidateErr: errors.New("hub unreachable")}
	h, prod := newHandlerWithAuditCount(db, spy)

	c, rec := echoCtx(http.MethodPost, "/quota-overrides", validCreateOverrideBody())
	if err := h.CreateQuotaOverride(c); err != nil {
		t.Fatalf("CreateQuotaOverride: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502; body=%s", rec.Code, rec.Body.String())
	}
	assertQuotaPropagationEnvelope(t, rec)
	if len(db.overrides) != 1 {
		t.Errorf("override not persisted on push failure: have %d; want 1", len(db.overrides))
	}
	if prod.seen() != 0 {
		t.Errorf("audit enqueues=%d; want 0", prod.seen())
	}
}

func TestUpdateQuotaOverride_HubFailure502(t *testing.T) {
	db := newFakeQuotaDB()
	ovr := sampleOverride()
	db.overrides[ovr.ID] = &ovr
	spy := &fakeHubAPI{invalidateErr: errors.New("hub down")}
	h, prod := newHandlerWithAuditCount(db, spy)

	c, rec := echoCtx(http.MethodPut, "/quota-overrides/"+ovr.ID, `{"reason":"test reason"}`)
	c.SetParamNames("id")
	c.SetParamValues(ovr.ID)
	if err := h.UpdateQuotaOverride(c); err != nil {
		t.Fatalf("UpdateQuotaOverride: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502; body=%s", rec.Code, rec.Body.String())
	}
	assertQuotaPropagationEnvelope(t, rec)
	if prod.seen() != 0 {
		t.Errorf("audit enqueues=%d; want 0", prod.seen())
	}
}

func TestDeleteQuotaOverride_HubFailure502(t *testing.T) {
	db := newFakeQuotaDB()
	ovr := sampleOverride()
	db.overrides[ovr.ID] = &ovr
	spy := &fakeHubAPI{invalidateErr: errors.New("hub down")}
	h, prod := newHandlerWithAuditCount(db, spy)

	c, rec := echoCtx(http.MethodDelete, "/quota-overrides/"+ovr.ID, "")
	c.SetParamNames("id")
	c.SetParamValues(ovr.ID)
	if err := h.DeleteQuotaOverride(c); err != nil {
		t.Fatalf("DeleteQuotaOverride: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502; body=%s", rec.Code, rec.Body.String())
	}
	assertQuotaPropagationEnvelope(t, rec)
	if _, stillThere := db.overrides[ovr.ID]; stillThere {
		t.Error("override should have been deleted before the push (DB is source of truth)")
	}
	if prod.seen() != 0 {
		t.Errorf("audit enqueues=%d; want 0", prod.seen())
	}
}

func assertQuotaPropagationEnvelope(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	m := decodeBody(t, rec)
	envAny, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error envelope: %v", m)
	}
	if envAny["type"] != "propagation_error" {
		t.Errorf("error.type = %v; want propagation_error", envAny["type"])
	}
	if envAny["code"] != "HUB_PROPAGATION_FAILED" {
		t.Errorf("error.code = %v; want HUB_PROPAGATION_FAILED", envAny["code"])
	}
}
