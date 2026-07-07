package audit

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"sync"
	"testing"
	"time"

	"log/slog"

	sharedndjson "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/ndjson"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// memProducer is a simple in-memory MQ producer for testing.
type memProducer struct {
	mu       sync.Mutex
	messages []memMsg
	failNext bool
	// failCount, when > 0, makes the next failCount Enqueue calls
	// return an error instead of accepting the message; subsequent
	// calls succeed. Used by Close() drain tests to simulate a
	// transient MQ outage that recovers within the deadline.
	failCount int
	// alwaysFail makes every Enqueue call return an error. Used to
	// simulate a sustained MQ outage that exceeds the drain deadline.
	alwaysFail bool
}

type memMsg struct {
	queue string
	data  []byte
}

func (p *memProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (p *memProducer) Enqueue(_ context.Context, queue string, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.alwaysFail {
		return context.DeadlineExceeded
	}
	if p.failCount > 0 {
		p.failCount--
		return context.DeadlineExceeded
	}
	if p.failNext {
		p.failNext = false
		return context.DeadlineExceeded
	}
	p.messages = append(p.messages, memMsg{queue: queue, data: data})
	return nil
}
func (p *memProducer) Close() error { return nil }
func (p *memProducer) msgs() []memMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]memMsg, len(p.messages))
	copy(cp, p.messages)
	return cp
}

func TestRecordToMessage_AllFields(t *testing.T) {
	logger := slog.Default()
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, logger)

	rec := &Record{
		RequestID:          "req-123",
		ClientRequestID:    "ext-req-456",
		TraceID:            "trace-upstream-789",
		Timestamp:          time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC),
		Method:             "POST",
		Path:               "/v1/chat/completions",
		StatusCode:         200,
		LatencyMs:          150,
		SourceIP:           "10.0.0.1",
		TargetHost:         "api.openai.com",
		UserID:             "user-1",
		UserDisplayName:    "Alice",
		OrganizationID:     "org-1",
		OrganizationName:   "AcmeCorp",
		ProjectID:          "proj-1",
		ProjectName:        "AI Chat",
		VirtualKeyID:       "vk-1",
		VirtualKeyName:     "demo-key",
		VKType:             "personal",
		CredentialID:       "cred-1",
		CredentialName:     "openai-key",
		ProviderID:         "openai",
		ProviderName:       "OpenAI",
		ModelID:            "gpt-4",
		ModelName:          "GPT-4",
		PromptTokens:       100,
		CompletionTokens:   50,
		TotalTokens:        150,
		EstimatedCostUsd:   0.05,
		CacheStatus:        CacheStatusMiss,
		RoutedProviderID:   "openai",
		RoutedProviderName: "OpenAI",
		RoutedModelID:      "gpt-4",
		RoutedModelName:    "GPT-4",
		RoutingRuleID:      "rule-1",
		RoutingRuleName:    "default",
		HookDecision:       "ALLOW",
		ComplianceTags:     []string{"severity:public"},
	}

	msg := w.recordToMessage(rec)

	if msg.ID != "req-123" {
		t.Errorf("ID = %q, want %q", msg.ID, "req-123")
	}
	if msg.Source != "ai-gateway" {
		t.Errorf("Source = %q, want %q", msg.Source, "ai-gateway")
	}
	if msg.TraceID != "trace-upstream-789" {
		t.Errorf("TraceID = %q, want %q", msg.TraceID, "trace-upstream-789")
	}
	if msg.ExternalRequestID != "ext-req-456" {
		t.Errorf("ExternalRequestID = %q, want %q", msg.ExternalRequestID, "ext-req-456")
	}
	if msg.EntityType != "user" {
		t.Errorf("EntityType = %q, want %q", msg.EntityType, "user")
	}
	if msg.EntityID != "user-1" {
		t.Errorf("EntityID = %q, want %q", msg.EntityID, "user-1")
	}
	if msg.PromptTokens != 100 {
		t.Errorf("PromptTokens = %d, want %d", msg.PromptTokens, 100)
	}
	if msg.TargetHost != "api.openai.com" {
		t.Errorf("TargetHost = %q, want %q", msg.TargetHost, "api.openai.com")
	}
	if msg.CacheStatus != string(CacheStatusMiss) {
		t.Errorf("CacheStatus = %q, want %q", msg.CacheStatus, CacheStatusMiss)
	}

	identity := jsonbMap(t, msg.Identity)
	if user, ok := identity["user"].(map[string]any); ok {
		if user["id"] != "user-1" {
			t.Errorf("Identity.user.id = %v, want %q", user["id"], "user-1")
		}
	} else {
		t.Error("Identity.user not found")
	}
	if vk, ok := identity["vk"].(map[string]any); ok {
		if vk["id"] != "vk-1" {
			t.Errorf("Identity.vk.id = %v, want %q", vk["id"], "vk-1")
		}
	} else {
		t.Error("Identity.vk not found")
	}
	if _, hasOldKey := identity["credential"]; hasOldKey {
		t.Error("Identity.credential should not exist — renamed to identity.vk")
	}
	// A record with a UserID resolves to status="matched" — already
	// resolved at request time, the Hub IdentityEnricher job's pending
	// filter must skip these.
	if identity["status"] != "matched" {
		t.Errorf("Identity.status = %v, want \"matched\" when UserID set", identity["status"])
	}
}

// TestRecordToMessage_ApplicationVKDispatchesToProject pins the
// application-VK branch: caller resolves to a Project, not a
// NexusUser. EntityType must be "project" (not "user") and EntityID
// must be the Project.id (not the VK name). identity.user MUST be
// absent — application VKs don't carry a NexusUser. Pre-fix this
// branch was misrouted: EntityType was hardcoded "user" and entity_id
// stored the VK name, so application-VK traffic was un-joinable to
// NexusUser AND was misclassified as user-traffic in dashboards.
func TestRecordToMessage_ApplicationVKDispatchesToProject(t *testing.T) {
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default())
	rec := &Record{
		RequestID:      "req-app-vk",
		Timestamp:      time.Unix(1700000000, 0),
		Method:         "POST",
		Path:           "/v1/chat/completions",
		StatusCode:     200,
		VirtualKeyID:   "vk-app-1",
		VirtualKeyName: "research-all-models",
		VKType:         "application",
		ProjectID:      "proj-research",
		ProjectName:    "Research",
		// UserID intentionally empty — application VKs don't carry one.
	}
	msg := w.recordToMessage(rec)

	if msg.EntityType != "project" {
		t.Errorf("EntityType = %q, want %q", msg.EntityType, "project")
	}
	if msg.EntityID != "proj-research" {
		t.Errorf("EntityID = %q, want %q (Project.id, NOT VK name)", msg.EntityID, "proj-research")
	}
	if msg.EntityName != "Research" {
		t.Errorf("EntityName = %q, want %q", msg.EntityName, "Research")
	}

	identity := jsonbMap(t, msg.Identity)
	if _, hasUser := identity["user"]; hasUser {
		t.Error("identity.user must be absent for application VK")
	}
	if proj, ok := identity["project"].(map[string]any); !ok || proj["id"] != "proj-research" {
		t.Errorf("identity.project missing or wrong: %v", identity["project"])
	}
	if vk, ok := identity["vk"].(map[string]any); !ok || vk["id"] != "vk-app-1" {
		t.Errorf("identity.vk missing or wrong: %v", identity["vk"])
	}
	if identity["status"] != "matched" {
		t.Errorf("identity.status = %v, want \"matched\" when Project resolved", identity["status"])
	}
}

// TestRecordToMessage_PersonalVKPopulatesUserNotProject mirrors the
// application case for completeness: VKType="personal" → user
// dispatch. Distinct from TestRecordToMessage_AllFields above because
// that test sets both UserID and ProjectID; this one explicitly
// verifies the personal branch wins when both are present.
func TestRecordToMessage_PersonalVKPopulatesUserNotProject(t *testing.T) {
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default())
	rec := &Record{
		RequestID:       "req-personal-vk",
		Timestamp:       time.Unix(1700000000, 0),
		Method:          "POST",
		Path:            "/v1/chat/completions",
		StatusCode:      200,
		VirtualKeyID:    "vk-personal-1",
		VirtualKeyName:  "my-laptop",
		VKType:          "personal",
		UserID:          "nexus-user-david",
		UserDisplayName: "David Thompson",
		// Personal VKs may also have a ProjectID (project the user is
		// in) but EntityType must dispatch to "user", not "project".
		ProjectID:   "proj-research",
		ProjectName: "Research",
	}
	msg := w.recordToMessage(rec)

	if msg.EntityType != "user" {
		t.Errorf("EntityType = %q, want %q (personal VK must dispatch to user)", msg.EntityType, "user")
	}
	if msg.EntityID != "nexus-user-david" {
		t.Errorf("EntityID = %q, want NexusUser.id", msg.EntityID)
	}
	identity := jsonbMap(t, msg.Identity)
	if _, hasUser := identity["user"]; !hasUser {
		t.Error("identity.user missing for personal VK")
	}
}

// TestRecordToMessage_StampsPendingWhenNoUser verifies ai-gateway rows
// that ran without a NexusUser (raw API key, no VK match) ship with
// identity.status="pending" so the Hub IdentityEnricher's
// ip_address-based resolver gets a chance.
func TestRecordToMessage_StampsPendingWhenNoUser(t *testing.T) {
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default())
	rec := &Record{
		RequestID:  "req-no-user",
		Timestamp:  time.Unix(1700000000, 0),
		Method:     "POST",
		Path:       "/v1/chat/completions",
		StatusCode: 200,
		// Deliberately no UserID / VirtualKeyID / CredentialID / ProjectID.
	}
	msg := w.recordToMessage(rec)
	if msg.Identity == nil {
		t.Fatal("Identity is nil; producer must stamp {status:pending}")
	}
	identity := jsonbMap(t, msg.Identity)
	if identity["status"] != "pending" {
		t.Errorf("Identity.status = %v, want \"pending\" when no user context", identity["status"])
	}
	if _, hasUser := identity["user"]; hasUser {
		t.Error("Identity.user should not be populated when UserID is empty")
	}
}

func TestRecordToMessage_ThingIdentityStamped(t *testing.T) {
	// WithThingIdentity stamps ThingID/ThingName onto every emitted
	// TrafficEventMessage; the Hub db-writer scans these onto
	// traffic_event.thing_id / thing_name.
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default()).
		WithThingIdentity("gw-test-3050", "test-host")
	msg := w.recordToMessage(&Record{RequestID: "req-thing", Timestamp: time.Now()})
	if msg.ThingID != "gw-test-3050" {
		t.Errorf("ThingID = %q, want gw-test-3050", msg.ThingID)
	}
	if msg.ThingName != "test-host" {
		t.Errorf("ThingName = %q, want test-host", msg.ThingName)
	}

	// Without WithThingIdentity, both fields stay empty (the consumer
	// stores SQL NULL). Mirrors test/dev callers that don't wire identity.
	w2 := NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default())
	msg2 := w2.recordToMessage(&Record{RequestID: "req-no-thing", Timestamp: time.Now()})
	if msg2.ThingID != "" || msg2.ThingName != "" {
		t.Errorf("default Thing identity should be empty; got (%q,%q)", msg2.ThingID, msg2.ThingName)
	}
}

func TestRecordToMessage_TargetHostFallbacksToRoutedProviderName(t *testing.T) {
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default())
	msg := w.recordToMessage(&Record{
		RequestID:          "req-fallback",
		Timestamp:          time.Now(),
		RoutedProviderName: "moonshot",
	})
	if msg.TargetHost != "moonshot" {
		t.Fatalf("TargetHost = %q, want routed provider fallback", msg.TargetHost)
	}
}

func TestRecordToMessage_E18Fields(t *testing.T) {
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default())

	rec := &Record{
		RequestID:             "req-e18",
		Timestamp:             time.Now(),
		APIKeyClass:           "nvk_",
		APIKeyFingerprint:     "a1b2c3d4e5f60718",
		UsageExtractionStatus: "streaming_reported",
	}

	msg := w.recordToMessage(rec)

	if msg.APIKeyClass != "nvk_" {
		t.Errorf("APIKeyClass = %q, want nvk_", msg.APIKeyClass)
	}
	if msg.APIKeyFingerprint != "a1b2c3d4e5f60718" {
		t.Errorf("APIKeyFingerprint = %q, want a1b2c3d4e5f60718", msg.APIKeyFingerprint)
	}
	if msg.UsageExtractionStatus != "streaming_reported" {
		t.Errorf("UsageExtractionStatus = %q, want streaming_reported", msg.UsageExtractionStatus)
	}
}

func TestRecordToMessage_HookRewriteFields(t *testing.T) {
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default())

	// Not rewritten → keys must be absent so consumers can distinguish
	// "no rewrite" from "rewrote zero slots".
	msg := w.recordToMessage(&Record{RequestID: "r1", Timestamp: time.Now()})
	details := jsonbMap(t, msg.Details)
	if _, ok := details["hookRewritten"]; ok {
		t.Errorf("hookRewritten should be absent when not rewritten, got %v", details["hookRewritten"])
	}
	if _, ok := details["hookRewriteCount"]; ok {
		t.Errorf("hookRewriteCount should be absent when not rewritten")
	}

	// Rewritten → both keys present with expected values.
	msg = w.recordToMessage(&Record{RequestID: "r2", Timestamp: time.Now(), HookRewritten: true, HookRewriteCount: 3})
	details = jsonbMap(t, msg.Details)
	if got, ok := details["hookRewritten"].(bool); !ok || !got {
		t.Errorf("hookRewritten = %v, want true", details["hookRewritten"])
	}
	if got, ok := details["hookRewriteCount"].(float64); !ok || got != 3 {
		t.Errorf("hookRewriteCount = %v, want 3", details["hookRewriteCount"])
	}
}

func TestRecordToMessage_ResponseHookRewriteFields(t *testing.T) {
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default())

	msg := w.recordToMessage(&Record{RequestID: "r1", Timestamp: time.Now()})
	details := jsonbMap(t, msg.Details)
	if _, ok := details["responseHookRewritten"]; ok {
		t.Errorf("responseHookRewritten should be absent when not rewritten")
	}

	msg = w.recordToMessage(&Record{
		RequestID:                "r2",
		Timestamp:                time.Now(),
		ResponseHookRewritten:    true,
		ResponseHookRewriteCount: 2,
	})
	details = jsonbMap(t, msg.Details)
	if got, ok := details["responseHookRewritten"].(bool); !ok || !got {
		t.Errorf("responseHookRewritten = %v, want true", details["responseHookRewritten"])
	}
	if got, ok := details["responseHookRewriteCount"].(float64); !ok || got != 2 {
		t.Errorf("responseHookRewriteCount = %v, want 2", details["responseHookRewriteCount"])
	}
}

// TestRecordToMessage_BlockingRule verifies that a Record carrying a
// rule-pack attribution serialises to the wire as a non-nil
// TrafficEventMessage.RequestBlockingRule payload with the same pack / version /
// rule_id tuple. This is the contract the Hub db-writer relies on to
// persist the jsonb column `traffic_event.blocking_rule`.
func TestRecordToMessage_BlockingRule(t *testing.T) {
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default())

	msg := w.recordToMessage(&Record{RequestID: "r1", Timestamp: time.Now()})
	if msg.RequestBlockingRule != nil {
		t.Errorf("BlockingRule should be nil when not set; got %s", string(*msg.RequestBlockingRule))
	}

	rec := &Record{
		RequestID: "r2",
		Timestamp: time.Now(),
		BlockingRule: &rulepack.BlockingRule{
			Pack:        "content-safety",
			PackVersion: "1.0.0",
			RuleID:      "violence-kill",
		},
	}
	msg = w.recordToMessage(rec)
	if msg.RequestBlockingRule == nil {
		t.Fatal("BlockingRule should be set on the wire message")
	}
	var decoded rulepack.BlockingRule
	if err := json.Unmarshal(*msg.RequestBlockingRule, &decoded); err != nil {
		t.Fatalf("unmarshal BlockingRule: %v", err)
	}
	if decoded.Pack != "content-safety" ||
		decoded.PackVersion != "1.0.0" ||
		decoded.RuleID != "violence-kill" {
		t.Errorf("BlockingRule payload = %+v, want (content-safety, 1.0.0, violence-kill)", decoded)
	}
}

// consumerView mirrors the subset of the Hub consumer-side
// TrafficEventMessage (packages/nexus-hub/internal/observability/consumer/message.go)
// that this fix is about. The JSON tags MUST match the wire tags exactly so a
// round-trip through json proves the consumer reads the TYPED columns rather
// than digging into the details JSONB. Kept local to avoid a cross-module
// import of the Hub package.
type consumerView struct {
	RequestHookDecision    string           `json:"requestHookDecision,omitempty"`
	RequestHookReason      string           `json:"requestHookReason,omitempty"`
	RequestHookReasonCode  string           `json:"requestHookReasonCode,omitempty"`
	ResponseHookDecision   string           `json:"responseHookDecision,omitempty"`
	ResponseHookReason     string           `json:"responseHookReason,omitempty"`
	ResponseHookReasonCode string           `json:"responseHookReasonCode,omitempty"`
	RequestBlockingRule    *json.RawMessage `json:"requestBlockingRule,omitempty"`
	ResponseBlockingRule   *json.RawMessage `json:"responseBlockingRule,omitempty"`
}

func marshalForConsumer(t *testing.T, msg *mq.TrafficEventMessage) consumerView {
	t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal wire message: %v", err)
	}
	var cv consumerView
	if err := json.Unmarshal(b, &cv); err != nil {
		t.Fatalf("unmarshal into consumer view: %v", err)
	}
	return cv
}

// A response-stage block must populate the TYPED response columns
// (response_hook_reason / response_hook_reason_code / response_blocking_rule)
// and must NOT pollute the request-stage blocking-rule column.
func TestRecordToMessage_ResponseStageBlock(t *testing.T) {
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default())

	rec := &Record{
		RequestID:              "resp-block",
		Timestamp:              time.Now(),
		ResponseHookDecision:   string(decision.RejectHard),
		ResponseHookReason:     "leaked secret in completion",
		ResponseHookReasonCode: "PII_EGRESS",
		BlockingRule: &rulepack.BlockingRule{
			Pack:        "egress-dlp",
			PackVersion: "2.1.0",
			RuleID:      "secret-key",
		},
	}
	msg := w.recordToMessage(rec)

	if msg.ResponseHookReason != "leaked secret in completion" {
		t.Errorf("ResponseHookReason = %q, want typed mapping", msg.ResponseHookReason)
	}
	if msg.ResponseHookReasonCode != "PII_EGRESS" {
		t.Errorf("ResponseHookReasonCode = %q, want typed mapping", msg.ResponseHookReasonCode)
	}
	if msg.ResponseBlockingRule == nil {
		t.Fatal("ResponseBlockingRule must be set for a response-stage block")
	}
	if msg.RequestBlockingRule != nil {
		t.Errorf("RequestBlockingRule must be nil for a response-stage block; got %s",
			string(*msg.RequestBlockingRule))
	}
	var decoded rulepack.BlockingRule
	if err := json.Unmarshal(*msg.ResponseBlockingRule, &decoded); err != nil {
		t.Fatalf("unmarshal ResponseBlockingRule: %v", err)
	}
	if decoded.Pack != "egress-dlp" || decoded.RuleID != "secret-key" {
		t.Errorf("ResponseBlockingRule payload = %+v, want (egress-dlp, secret-key)", decoded)
	}

	// Spot-assert the Hub consumer reads the typed columns off the wire, not
	// the details JSONB.
	cv := marshalForConsumer(t, msg)
	if cv.ResponseHookReason != "leaked secret in completion" || cv.ResponseHookReasonCode != "PII_EGRESS" {
		t.Errorf("consumer view response reason/code = %q/%q, want typed values",
			cv.ResponseHookReason, cv.ResponseHookReasonCode)
	}
	if cv.ResponseBlockingRule == nil {
		t.Error("consumer view ResponseBlockingRule must be populated")
	}
	if cv.RequestBlockingRule != nil {
		t.Error("consumer view RequestBlockingRule must be empty for a response-stage block")
	}
}

// A request-stage block routes to the request columns only; the
// response-stage typed columns stay empty.
func TestRecordToMessage_RequestStageBlock(t *testing.T) {
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default())

	rec := &Record{
		RequestID:      "req-block",
		Timestamp:      time.Now(),
		HookDecision:   string(decision.RejectHard),
		HookReason:     "prompt contains banned phrase",
		HookReasonCode: "PROMPT_BANNED",
		BlockingRule: &rulepack.BlockingRule{
			Pack:        "input-guard",
			PackVersion: "1.2.0",
			RuleID:      "banned-phrase",
		},
	}
	msg := w.recordToMessage(rec)

	if msg.RequestHookReason != "prompt contains banned phrase" || msg.RequestHookReasonCode != "PROMPT_BANNED" {
		t.Errorf("request reason/code = %q/%q, want typed mapping",
			msg.RequestHookReason, msg.RequestHookReasonCode)
	}
	if msg.RequestBlockingRule == nil {
		t.Fatal("RequestBlockingRule must be set for a request-stage block")
	}
	if msg.ResponseBlockingRule != nil {
		t.Errorf("ResponseBlockingRule must be nil for a request-stage block; got %s",
			string(*msg.ResponseBlockingRule))
	}
	if msg.ResponseHookReason != "" || msg.ResponseHookReasonCode != "" {
		t.Errorf("response typed columns must be empty for a request-stage block; got %q/%q",
			msg.ResponseHookReason, msg.ResponseHookReasonCode)
	}
	var decoded rulepack.BlockingRule
	if err := json.Unmarshal(*msg.RequestBlockingRule, &decoded); err != nil {
		t.Fatalf("unmarshal RequestBlockingRule: %v", err)
	}
	if decoded.Pack != "input-guard" || decoded.RuleID != "banned-phrase" {
		t.Errorf("RequestBlockingRule payload = %+v, want (input-guard, banned-phrase)", decoded)
	}

	cv := marshalForConsumer(t, msg)
	if cv.RequestBlockingRule == nil {
		t.Error("consumer view RequestBlockingRule must be populated")
	}
	if cv.ResponseBlockingRule != nil {
		t.Error("consumer view ResponseBlockingRule must be empty for a request-stage block")
	}
}

// A BLOCK_SOFT response decision (decision="blocked"-equivalent soft
// reject) also carries its rule attribution to the response column, not only
// to details.
func TestRecordToMessage_ResponseSoftBlockRoutesToResponseColumn(t *testing.T) {
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default())

	rec := &Record{
		RequestID:              "resp-soft",
		Timestamp:              time.Now(),
		ResponseHookDecision:   string(decision.BlockSoft),
		ResponseHookReason:     "soft compliance flag on output",
		ResponseHookReasonCode: "SOFT_FLAG",
		BlockingRule: &rulepack.BlockingRule{
			Pack:        "compliance",
			PackVersion: "3.0.0",
			RuleID:      "advisory-1",
		},
	}
	msg := w.recordToMessage(rec)

	if msg.ResponseBlockingRule == nil {
		t.Fatal("ResponseBlockingRule must be set for a BLOCK_SOFT response decision")
	}
	if msg.RequestBlockingRule != nil {
		t.Errorf("RequestBlockingRule must stay nil for a response soft block; got %s",
			string(*msg.RequestBlockingRule))
	}
	cv := marshalForConsumer(t, msg)
	if cv.ResponseHookReason != "soft compliance flag on output" {
		t.Errorf("consumer view response reason = %q, want typed soft-block reason", cv.ResponseHookReason)
	}
}

func TestIsBlockingDecision(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{string(decision.RejectHard), true},
		{string(decision.BlockSoft), true},
		{string(decision.Approve), false},
		{string(decision.Modify), false},
		{string(decision.Abstain), false},
		{"", false},
		{"BYPASSED", false},
	}
	for _, c := range cases {
		if got := isBlockingDecision(c.in); got != c.want {
			t.Errorf("isBlockingDecision(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestFlushToMQ(t *testing.T) {
	prod := &memProducer{}
	logger := slog.Default()
	w := NewWriter(prod, "nexus.event.ai-traffic", nil, logger).Start()
	defer w.Close()

	for i := range 3 {
		w.Enqueue(&Record{
			RequestID:  "req-" + string(rune('A'+i)),
			Timestamp:  time.Now(),
			Method:     "POST",
			StatusCode: 200,
		})
	}

	// The consumer workers drain recCh and publish each record on their pool
	// connection (memProducer is a non-batch producer → per-record publishRecord).
	if got := waitForMsgCount(prod, 3, 2*time.Second); got != 3 {
		t.Fatalf("expected 3 messages, got %d", got)
	}

	for _, m := range prod.msgs() {
		if m.queue != "nexus.event.ai-traffic" {
			t.Errorf("queue = %q, want %q", m.queue, "nexus.event.ai-traffic")
		}
		var msg mq.TrafficEventMessage
		if err := json.Unmarshal(m.data, &msg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if msg.Source != "ai-gateway" {
			t.Errorf("Source = %q, want %q", msg.Source, "ai-gateway")
		}
	}
}

func TestFlushMQFailure_RetryReQueue(t *testing.T) {
	prod := &memProducer{failNext: true} // first publish fails, then recovers
	logger := slog.Default()
	w := NewWriter(prod, "nexus.event.ai-traffic", nil, logger).Start()
	defer w.Close()

	w.Enqueue(&Record{RequestID: "fail-me", Timestamp: time.Now()})

	// The first publish fails → the record is re-queued onto recCh (never lost) and a
	// consumer worker retries it; the second publish succeeds, so the record lands.
	if got := waitForMsgCount(prod, 1, 2*time.Second); got != 1 {
		t.Errorf("expected 1 message after transient-failure retry, got %d", got)
	}
}

func TestNoOpMode(t *testing.T) {
	logger := slog.Default()
	// Nil producer → records are enqueued, drained by the consumer, and discarded
	// at publish (publishBatchOn returns early on a nil producer). Must not panic.
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, logger).Start()
	defer w.Close()

	w.Enqueue(&Record{RequestID: "noop-1", Timestamp: time.Now()})
}

// TestCloseDrain_RecoversFromTransientFailure pins the graceful-shutdown
// contract: when MQ blips during the final drain, the consumer's re-queue retry
// must keep trying until the queue empties so no record is silently dropped on a
// normal SIGTERM.
func TestCloseDrain_RecoversFromTransientFailure(t *testing.T) {
	prod := &memProducer{failCount: 2} // first 2 publishes fail, then recover
	w := NewWriter(prod, "nexus.event.ai-traffic", nil, slog.Default()).Start()

	w.Enqueue(&Record{RequestID: "rec-1", Timestamp: time.Now()})

	// A publish failure re-queues the record onto recCh; the live consumer worker
	// keeps re-pulling and retrying until the transient outage clears, so the record
	// eventually lands — no record lost to a transient blip.
	if got := waitForMsgCount(prod, 1, 2*time.Second); got != 1 {
		t.Errorf("transient-failure retry should publish the record; got %d", got)
	}
	w.Close()
}

// TestPublishFailure_OverflowSpillsNotLost pins the sustained-outage no-loss
// contract under the producer/multi-consumer model: when a publish fails and the
// bounded queue cannot absorb the re-queued record (full), the record is spilled
// durably to the NDJSON sink rather than retried forever or silently dropped. With a
// durable spill wired, alwaysFail traffic surfaces on disk, never on the (failing)
// producer.
func TestPublishFailure_OverflowSpillsNotLost(t *testing.T) {
	dir := t.TempDir()
	spill, err := sharedndjson.New(dir, "test", 64, 512, nil)
	if err != nil {
		t.Fatalf("ndjson.New: %v", err)
	}
	prod := &memProducer{alwaysFail: true}
	w := NewWriter(prod, "nexus.event.ai-traffic", nil, slog.Default()).WithNDJSONSpill(spill)
	// A full bounded queue forces handlePublishFailure's non-blocking re-queue to
	// fail, routing the un-publishable record to the durable spill (the bounded
	// no-loss fallback for a wedged pipeline).
	w.recCh = make(chan *Record, 1)
	w.recCh <- &Record{RequestID: "fill"} // saturate (cap 1)

	const n = 3
	batch := make([]*Record, n)
	for i := range batch {
		batch[i] = &Record{RequestID: fmt.Sprintf("rec-stuck-%d", i), Timestamp: time.Now().Add(time.Duration(i))}
	}
	w.publishBatchOn(0, batch) // every publish fails → re-queue full → durable batched spill

	if got := len(prod.msgs()); got != 0 {
		t.Errorf("alwaysFail producer must not record messages; got %d", got)
	}
	// The stuck records were handed to the batched spill channel (off the publish
	// worker) rather than written synchronously; drive the worker's write step for
	// each and confirm they land durably — no silent loss, no infinite retry.
	for i := range n {
		select {
		case rec := <-w.spillCh:
			if !w.spillRecord(rec) {
				t.Fatalf("spillRecord must durably write stuck record %d", i)
			}
		default:
			t.Fatalf("stuck record %d was not handed to the spill channel", i)
		}
	}
	if got := len(readSpool(t, dir, "test")); got != n {
		t.Errorf("sustained-outage records must spill durably; spooled %d, want %d", got, n)
	}
}

func TestBufferOverflow(t *testing.T) {
	logger := slog.Default()
	w := NewWriter(nil, "nexus.event.ai-traffic", nil, logger).WithLossMode(lossModeDrop)
	if w.lossMode != lossModeDrop {
		t.Fatalf("lossMode = %q, want drop", w.lossMode)
	}
	// Claim startOnce so Enqueue's lazy ensureStarted() does not spin up consumers
	// that would drain the queue, then saturate the bounded queue to its cap.
	w.startOnce.Do(func() {})
	const cap = 8
	w.recCh = make(chan *Record, cap)
	for range cap {
		w.recCh <- &Record{RequestID: "fill", Timestamp: time.Now()}
	}

	// In drop mode, every Enqueue past the cap is a counted bounded drop — the queue
	// never grows past its capacity.
	for range cap + 10 {
		w.Enqueue(&Record{RequestID: "overflow", Timestamp: time.Now()})
	}

	if count := len(w.recCh); count > cap {
		t.Errorf("queue size %d exceeds cap %d", count, cap)
	}
}
