package helpers

import (
	"context"
	"net/http"
)

// AIGwPostJSON does a POST <env.AIGwURL><path> with the test VK and a
// JSON body, returning (status, body, err). Status code is returned even
// on non-2xx so tests can assert rejection paths (401, 403, 451).
func AIGwPostJSON(env *Env, client *http.Client, path string, body []byte) (int, []byte, error) {
	return DoJSON(client, context.Background(), http.MethodPost,
		env.AIGwURL+path, "Bearer "+env.TestVK, body)
}

// AIGwGet does a GET against the AI Gateway with the test VK.
func AIGwGet(env *Env, client *http.Client, path string) (int, []byte, error) {
	return DoJSON(client, context.Background(), http.MethodGet,
		env.AIGwURL+path, "Bearer "+env.TestVK, nil)
}
