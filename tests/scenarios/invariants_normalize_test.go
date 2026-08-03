// Cross-cutting invariant — cross-ingress normalize parity.
//
// The normalize layer canonicalizes every ingress's wire format into one shape
// (kind=ai-chat + messages[].content[].text). The same logical prompt sent via
// two different ingresses must produce the SAME canonical normalized request —
// otherwise an ingress is silently dropping or mangling the prompt on the way
// into the compliance / analytics pipeline.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// normReq is the slice of traffic_event_normalized.request_normalized this
// invariant inspects. Both the openai-chat and openai-responses ingresses
// canonicalize to this same shape.
type normReq struct {
	Kind             string `json:"kind"`
	Model            string `json:"model"`
	NormalizeVersion string `json:"normalizeVersion"`
	Messages         []struct {
		Role    string `json:"role"`
		Content []struct {
			Text string `json:"text"`
			Type string `json:"type"`
		} `json:"content"`
	} `json:"messages"`
}

func (n normReq) firstUserText() string {
	for _, m := range n.Messages {
		if m.Role == "user" {
			for _, c := range m.Content {
				if c.Type == "text" && c.Text != "" {
					return c.Text
				}
			}
		}
	}
	return ""
}

// TestS152_CrossIngressNormalizeParity — cross-cutting normalize invariant.
//
// Cross-service: AI Gw -> MQ -> traffic_event (raw body) -> CP's VIEW-TIME
// normalize projection, read through GET /api/admin/traffic/:id/normalized —
// the same surface the UI's Normalized tab uses. The same prompt via
// /v1/chat/completions and /v1/responses must canonicalize identically:
// kind=ai-chat, the same normalizeVersion, and the same extracted user text. A
// divergence means one ingress's normalizer drifted.
//
// This used to read traffic_event_normalized directly, and could not pass:
// normalization is VIEW-TIME, so that table is a purely OPTIONAL sidecar and is
// empty in this deployment (0 rows). The Hub writer skips any message carrying
// no normalized payload and no normalize status, which is every message the
// gateway sends now, and the flush path says so in its own comment — "normalized
// projection at view time from the surviving raw body, so a missing sidecar row
// does not lose the normalized view". Polling the sidecar for 40s therefore
// waited on a row the architecture had stopped producing, and reported it as
// "normalize pipeline lagged or did not run".
func TestS152_CrossIngressNormalizeParity(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}
	vkName := fmt.Sprintf("s152-%d", time.Now().UnixNano())
	vk, err := helpers.CreateMyVK(ctx, sc.Env, token, vkName)
	if err != nil {
		t.Fatalf("CreateMyVK: %v", err)
	}
	sc.Cleanup.Register("DeleteMyVK("+vk.ID+")", func() error {
		return helpers.DeleteMyVK(context.Background(), sc.Env, token, vk.ID)
	})

	envForCall := *sc.Env
	envForCall.TestVK = vk.RawKey
	client := intg.LocalHTTPClient()
	prompt := fmt.Sprintf("Normalize parity probe; reply OK. n=%d", time.Now().UnixNano())

	// Same logical prompt, two ingress wire shapes.
	chatBody := mustMarshal(t, map[string]any{
		"model":       "moonshot-v1-8k",
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens":  8,
		"temperature": 0,
	})
	respBody := mustMarshal(t, map[string]any{
		"model": "moonshot-v1-8k",
		"input": prompt,
		"store": false,
	})

	if s, b, err := intg.AIGwPostJSON(&envForCall, client, "/v1/chat/completions", chatBody); err != nil || s != 200 {
		t.Fatalf("chat send: status=%d err=%v body=%q", s, err, truncate(b, 200))
	}
	if s, b, err := intg.AIGwPostJSON(&envForCall, client, "/v1/responses", respBody); err != nil || s != 200 {
		t.Fatalf("responses send: status=%d err=%v body=%q", s, err, truncate(b, 200))
	}

	chatN := fetchNormalizedRequest(t, sc, ctx, token, vk.ID, "/v1/chat/completions")
	respN := fetchNormalizedRequest(t, sc, ctx, token, vk.ID, "/v1/responses")

	// Parity assertions.
	if chatN.Kind != "ai-chat" {
		t.Errorf("chat normalized kind=%q (want ai-chat)", chatN.Kind)
	}
	if respN.Kind != "ai-chat" {
		t.Errorf("responses normalized kind=%q (want ai-chat)", respN.Kind)
	}
	chatText, respText := chatN.firstUserText(), respN.firstUserText()
	if chatText != prompt {
		t.Errorf("chat normalizer altered/dropped the user prompt: got %q want %q", chatText, prompt)
	}
	if respText != prompt {
		t.Errorf("responses normalizer altered/dropped the user prompt: got %q want %q", respText, prompt)
	}
	if chatText != respText {
		t.Errorf("cross-ingress normalize divergence: chat=%q responses=%q for the same logical prompt", chatText, respText)
	}
	if chatN.NormalizeVersion != respN.NormalizeVersion {
		t.Errorf("normalizeVersion diverges across ingresses: chat=%q responses=%q", chatN.NormalizeVersion, respN.NormalizeVersion)
	}
	t.Logf("S-152 OK: kind=%s normalizeVersion=%s userText parity=%v (chat & responses normalize to one canonical shape)",
		chatN.Kind, chatN.NormalizeVersion, chatText == respText && chatText == prompt)
}

// fetchNormalizedRequest resolves the traffic_event for a VK+path and reads its
// normalized projection from the admin view-time endpoint. traffic_event IS
// written (the raw row is the source of truth); the projection is recomputed on
// read, so there is no sidecar row to wait for — only the raw row's own audit
// latency.
func fetchNormalizedRequest(t *testing.T, sc *scenarioCtx, ctx context.Context, token, vkID, path string) normReq {
	t.Helper()
	const query = `
		SELECT e.id
		FROM traffic_event e
		WHERE e.source = 'ai-gateway'
		  AND e.identity->'vk'->>'id' = $1
		  AND e.path = $2
		  AND e."timestamp" > NOW() - INTERVAL '300 seconds'
		ORDER BY e."timestamp" DESC
		LIMIT 1`
	const tries = 20
	const interval = 2 * time.Second
	var eventID string
	for i := 0; i < tries; i++ {
		if scanErr := sc.DB.QueryRow(ctx, query, vkID, path).Scan(&eventID); scanErr == nil && eventID != "" {
			break
		}
		time.Sleep(interval)
	}
	if eventID == "" {
		t.Fatalf("no ai-gateway traffic_event for path=%s VK=%s within %v — the request returned 200, so this says the audit row never landed",
			path, vkID, time.Duration(tries)*interval)
	}

	st, body, err := helpers.CPDoJSON(ctx, sc.Env, token, http.MethodGet,
		"/api/admin/traffic/"+eventID+"/normalized", nil)
	if err != nil {
		t.Fatalf("GET /normalized for event %s (path=%s): %v", eventID, path, err)
	}
	if st != http.StatusOK {
		t.Fatalf("GET /normalized for event %s (path=%s): status=%d body=%q",
			eventID, path, st, truncate(body, 240))
	}
	// The envelope is {requestStatus, responseStatus, requestNormalized,
	// responseNormalized}. requestStatus is asserted because a projection that
	// failed to compute still returns 200 with an empty payload — treating that
	// as "no divergence" would make the parity check vacuous.
	var doc struct {
		RequestStatus     string  `json:"requestStatus"`
		RequestNormalized normReq `json:"requestNormalized"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse /normalized for path=%s: %v (raw=%s)", path, err, truncate(body, 240))
	}
	if doc.RequestStatus != "ok" {
		t.Fatalf("view-time normalize did not succeed for path=%s: requestStatus=%q (raw=%s)",
			path, doc.RequestStatus, truncate(body, 240))
	}
	return doc.RequestNormalized
}
