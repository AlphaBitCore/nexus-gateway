package helpers

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

// AIGwPostJSON does a POST <env.AIGwURL><path> with the test VK and a
// JSON body, returning (status, body, err). Status code is returned even
// on non-2xx so tests can assert rejection paths (401, 403, 451).
func AIGwPostJSON(env *Env, client *http.Client, path string, body []byte) (int, []byte, error) {
	return DoJSON(client, context.Background(), http.MethodPost,
		env.AIGwURL+path, "Bearer "+env.TestVK, body)
}

// AIGwPostRSToken posts to an AI-Gateway route gated by rstokenauth, which reads
// the shared internal secret from the X-RS-Token HEADER rather than from
// Authorization. A route behind that middleware answers 401 RS_TOKEN_REQUIRED to
// any bearer, however valid — so a scenario that only knows how to send a bearer
// cannot reach one at all.
func AIGwPostRSToken(env *Env, client *http.Client, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		env.AIGwURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-RS-Token", env.HubServiceToken)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	return resp.StatusCode, out, err
}

// AIGwGet does a GET against the AI Gateway with the test VK.
func AIGwGet(env *Env, client *http.Client, path string) (int, []byte, error) {
	return DoJSON(client, context.Background(), http.MethodGet,
		env.AIGwURL+path, "Bearer "+env.TestVK, nil)
}
