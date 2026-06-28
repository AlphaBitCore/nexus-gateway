package cli

import (
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resourceServer records the last admin call and echoes a scripted body.
type resourceServer struct {
	*httptest.Server
	lastMethod string
	lastPath   string
	lastBody   string
	body       string
}

func newResourceServer(t *testing.T, body string) *resourceServer {
	rs := &resourceServer{body: body}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.lastMethod, rs.lastPath = r.Method, r.URL.Path
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			rs.lastBody = string(b)
		}
		_, _ = io.WriteString(w, rs.body)
	}))
	t.Cleanup(rs.Close)
	return rs
}

func TestResourceKinds(t *testing.T) {
	rs := newResourceServer(t, "")
	out, err := runCLI(t, newTestApp(rs.Server, false), "resource", "kinds")
	if err != nil {
		t.Fatalf("resource kinds: %v", err)
	}
	if !strings.Contains(out, "virtual-keys") || !strings.Contains(out, "CAPABILITIES") {
		t.Fatalf("kinds table:\n%s", out)
	}
	// The silent-kinds bug: every row must carry a non-empty profile — a
	// reports kind says "report", a singleton-config kind says "config".
	for _, want := range []string{"crud", "report", "config", "action:dry-run"} {
		if !strings.Contains(out, want) {
			t.Fatalf("kinds table missing capability %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "update update") {
		t.Fatalf("duplicate verbs leaked into the kinds table:\n%s", out)
	}
	out, _ = runCLI(t, newTestApp(rs.Server, false), "resource", "kinds", "-o", "json")
	var kinds []map[string]any
	if err := json.Unmarshal([]byte(out), &kinds); err != nil || len(kinds) == 0 {
		t.Fatalf("kinds json: %v\n%s", err, out)
	}
	for _, k := range kinds {
		caps, _ := k["capabilities"].([]any)
		if len(caps) == 0 {
			t.Fatalf("kind %v has empty capabilities in JSON output", k["kind"])
		}
	}
}

func TestResourceSearch(t *testing.T) {
	rs := newResourceServer(t, "")
	out, err := runCLI(t, newTestApp(rs.Server, false), "resource", "search", "node", "override")
	if err != nil || !strings.Contains(out, "setNodeOverride") {
		t.Fatalf("search should surface setNodeOverride:\n%s\n%v", out, err)
	}
	out, _ = runCLI(t, newTestApp(rs.Server, false), "resource", "search", "zzzznotathing")
	if !strings.Contains(out, "no operations match") {
		t.Fatalf("a non-matching search should say so:\n%s", out)
	}
}

func TestResourceDescribe(t *testing.T) {
	rs := newResourceServer(t, "")
	out, err := runCLI(t, newTestApp(rs.Server, false), "resource", "describe", "virtual-keys")
	if err != nil || !strings.Contains(out, "createVirtualKey") || !strings.Contains(out, "POST") {
		t.Fatalf("describe:\n%s\n%v", out, err)
	}
	// The table now carries the operation's OpenAPI summary and its PATH —
	// the spec semantics the old LABEL column hid behind a synthesized tail.
	if !strings.Contains(out, "SUMMARY") || !strings.Contains(out, "PATH") || strings.Contains(out, "LABEL") {
		t.Fatalf("describe must show SUMMARY+PATH and drop LABEL:\n%s", out)
	}
	if !strings.Contains(out, "Create a virtual key") {
		t.Fatalf("describe must surface the real OpenAPI summary:\n%s", out)
	}
	if !strings.Contains(out, "name*") {
		t.Fatalf("describe must keep required-field markers:\n%s", out)
	}
	// unknown kind → usage error (exit 2)
	_, err = runCLI(t, newTestApp(rs.Server, false), "resource", "describe", "nope")
	if exitCode(err) != 2 {
		t.Fatalf("unknown kind should be a usage error, got exit %d", exitCode(err))
	}
}

func TestResourceReadTableAndRecord(t *testing.T) {
	rs := newResourceServer(t, `[{"id":"vk1","name":"eng"},{"id":"vk2","name":"ops"}]`)
	out, err := runCLI(t, newTestApp(rs.Server, false), "resource", "read", "virtual-keys", "listVirtualKeys")
	if err != nil {
		t.Fatalf("read list: %v", err)
	}
	if !strings.Contains(out, "eng") || !strings.Contains(strings.ToUpper(out), "NAME") || !strings.Contains(out, "(2 rows)") {
		t.Fatalf("read should render a table:\n%s", out)
	}
	if rs.lastMethod != "GET" || rs.lastPath != "/api/admin/virtual-keys" {
		t.Fatalf("read built %s %s", rs.lastMethod, rs.lastPath)
	}
	// a record body renders as indented JSON
	rs2 := newResourceServer(t, `{"id":"vk1","enabled":true}`)
	out, _ = runCLI(t, newTestApp(rs2.Server, false), "resource", "read", "virtual-keys", "getVirtualKey", "--param", "id=vk1")
	if !strings.Contains(out, `"enabled"`) {
		t.Fatalf("a record should render as JSON:\n%s", out)
	}
	if rs2.lastPath != "/api/admin/virtual-keys/vk1" {
		t.Fatalf("read get built %s", rs2.lastPath)
	}
}

func TestResourceReadRejectsWriteOp(t *testing.T) {
	rs := newResourceServer(t, "")
	_, err := runCLI(t, newTestApp(rs.Server, false), "resource", "read", "virtual-keys", "createVirtualKey")
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "resource invoke") {
		t.Fatalf("read of a write op should be a usage error pointing at invoke, got %v", err)
	}
}

func TestResourceInvokeWritesWithBody(t *testing.T) {
	rs := newResourceServer(t, `{"id":"new"}`)
	out, err := runCLI(t, newTestApp(rs.Server, false), "resource", "invoke", "virtual-keys", "createVirtualKey", "--body", `{"name":"ci"}`, "--yes")
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if rs.lastMethod != "POST" || rs.lastPath != "/api/admin/virtual-keys" || !strings.Contains(rs.lastBody, `"name":"ci"`) {
		t.Fatalf("invoke built %s %s body=%s", rs.lastMethod, rs.lastPath, rs.lastBody)
	}
	if !strings.Contains(out, "new") {
		t.Fatalf("invoke should print the response:\n%s", out)
	}
	// two-param write resolves both placeholders
	rs2 := newResourceServer(t, `{}`)
	_, err = runCLI(t, newTestApp(rs2.Server, false), "resource", "invoke", "nodes", "setNodeOverride",
		"--param", "id=n1", "--param", "configKey=cache.ttl", "--body", `{"value":true}`, "--yes")
	if err != nil || rs2.lastPath != "/api/admin/nodes/n1/overrides/cache.ttl" {
		t.Fatalf("two-param invoke built %s (%v)", rs2.lastPath, err)
	}
}

func TestResourceInvokeGuards(t *testing.T) {
	rs := newResourceServer(t, "")
	// invoking a GET op is a usage error pointing at read
	_, err := runCLI(t, newTestApp(rs.Server, false), "resource", "invoke", "virtual-keys", "getVirtualKey", "--param", "id=x", "--yes")
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "resource read") {
		t.Fatalf("invoke of a read op should point at read, got %v", err)
	}
	// a write without --yes and no TTY refuses
	_, err = runCLI(t, newTestApp(rs.Server, false), "resource", "invoke", "virtual-keys", "createVirtualKey", "--body", `{"name":"x"}`)
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("a non-interactive write must require --yes, got %v", err)
	}
	if rs.lastMethod != "" {
		t.Fatal("a refused write must not reach the server")
	}
	// an invalid body fails before the call
	_, err = runCLI(t, newTestApp(rs.Server, false), "resource", "invoke", "virtual-keys", "createVirtualKey", "--body", `{not json`, "--yes")
	if exitCode(err) != 2 {
		t.Fatalf("invalid JSON body should be a usage error, got %v", err)
	}
	// --body and --body-file together is an error
	_, err = runCLI(t, newTestApp(rs.Server, false), "resource", "invoke", "virtual-keys", "createVirtualKey", "--body", `{}`, "--body-file", "x", "--yes")
	if exitCode(err) != 2 {
		t.Fatalf("--body + --body-file should conflict, got %v", err)
	}
	// unknown op
	_, err = runCLI(t, newTestApp(rs.Server, false), "resource", "read", "virtual-keys", "ghostOp")
	if exitCode(err) != 2 {
		t.Fatalf("unknown op should be a usage error, got %v", err)
	}
}

func TestResourceInvokeInteractiveConfirm(t *testing.T) {
	// y at the prompt applies the write.
	rs := newResourceServer(t, `{"ok":true}`)
	out, err := runCLIWithIn(t, newTestApp(rs.Server, false), "y\n",
		"resource", "invoke", "virtual-keys", "createVirtualKey", "--body", `{"name":"ci"}`)
	if err != nil || rs.lastMethod != "POST" {
		t.Fatalf("y should apply the write: %v (%s)", err, rs.lastMethod)
	}
	if !strings.Contains(out, "Apply POST") {
		t.Fatalf("the prompt should show the call:\n%s", out)
	}
	// n at the prompt aborts without calling.
	rs2 := newResourceServer(t, `{}`)
	out, err = runCLIWithIn(t, newTestApp(rs2.Server, false), "n\n",
		"resource", "invoke", "virtual-keys", "createVirtualKey", "--body", `{"name":"x"}`)
	if err != nil || rs2.lastMethod != "" || !strings.Contains(out, "aborted") {
		t.Fatalf("n should abort without calling: %v (%s)\n%s", err, rs2.lastMethod, out)
	}
	// prod prints a louder prompt.
	rs3 := newResourceServer(t, `{}`)
	out, _ = runCLIWithIn(t, newTestApp(rs3.Server, true), "y\n",
		"resource", "invoke", "virtual-keys", "createVirtualKey", "--body", `{"name":"x"}`)
	if !strings.Contains(out, "PRODUCTION") {
		t.Fatalf("a prod write prompt should name production:\n%s", out)
	}
}

func TestResourceInvokeBodyFileAndQuery(t *testing.T) {
	dir := t.TempDir()
	bf := filepath.Join(dir, "body.json")
	if err := os.WriteFile(bf, []byte(`{"name":"from-file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rs := newResourceServer(t, `{"id":"x"}`)
	_, err := runCLI(t, newTestApp(rs.Server, false), "resource", "invoke", "virtual-keys", "createVirtualKey", "--body-file", bf, "--yes")
	if err != nil || !strings.Contains(rs.lastBody, "from-file") {
		t.Fatalf("--body-file should be sent: %v body=%s", err, rs.lastBody)
	}
	// a missing body-file errors
	_, err = runCLI(t, newTestApp(rs.Server, false), "resource", "invoke", "virtual-keys", "createVirtualKey", "--body-file", filepath.Join(dir, "nope.json"), "--yes")
	if exitCode(err) != 2 {
		t.Fatalf("a missing body-file should be a usage error, got %v", err)
	}
	// read with a query param forwards it.
	rsq := newResourceServer(t, `[]`)
	out, _ := runCLI(t, newTestApp(rsq.Server, false), "resource", "read", "virtual-keys", "listVirtualKeys", "--query", "status=active")
	if !strings.Contains(out, "(empty)") {
		t.Fatalf("an empty list should say (empty):\n%s", out)
	}
	if !strings.Contains(rsq.lastPath, "virtual-keys") {
		t.Fatalf("query read built %s", rsq.lastPath)
	}
	// -o json read emits the raw body.
	rsj := newResourceServer(t, `[{"id":"a"}]`)
	out, _ = runCLI(t, newTestApp(rsj.Server, false), "resource", "read", "virtual-keys", "listVirtualKeys", "-o", "json")
	if !strings.Contains(out, `"id"`) {
		t.Fatalf("json read should emit the raw body:\n%s", out)
	}
	// describe -o json emits operation schemas.
	out, _ = runCLI(t, newTestApp(rs.Server, false), "resource", "describe", "virtual-keys", "-o", "json")
	if !strings.Contains(out, "createVirtualKey") {
		t.Fatalf("describe json:\n%s", out)
	}
}

func TestResourceInvokeNoBodyAndStdinAnd403(t *testing.T) {
	// An action with no body: resolveBody returns nothing; the POST still fires.
	rs := newResourceServer(t, `{"revoked":true}`)
	_, err := runCLI(t, newTestApp(rs.Server, false), "resource", "invoke", "virtual-keys", "revokeVirtualKey", "--param", "id=vk1", "--yes")
	if err != nil || rs.lastPath != "/api/admin/virtual-keys/vk1/revoke" {
		t.Fatalf("no-body action built %s (%v)", rs.lastPath, err)
	}
	// --body-file - reads the request body from stdin.
	rs2 := newResourceServer(t, `{}`)
	_, err = runCLIWithIn(t, newTestApp(rs2.Server, false), `{"name":"piped"}`,
		"resource", "invoke", "virtual-keys", "createVirtualKey", "--body-file", "-", "--yes")
	if err != nil || !strings.Contains(rs2.lastBody, "piped") {
		t.Fatalf("--body-file - should read stdin: %v body=%s", err, rs2.lastBody)
	}
	// search -o json emits the candidate list.
	out, _ := runCLI(t, newTestApp(rs.Server, false), "resource", "search", "node", "-o", "json")
	if !strings.Contains(out, "operationId") {
		t.Fatalf("search json:\n%s", out)
	}
	// a 403 from the server maps to exit 4 (read path surfaces the transport error).
	srv403 := newResourceServer(t, "")
	srv403.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"denied"}}`)
	})
	_, err = runCLI(t, newTestApp(srv403.Server, false), "resource", "read", "virtual-keys", "listVirtualKeys")
	if exitCode(err) != 4 {
		t.Fatalf("a 403 should map to exit 4, got %d (%v)", exitCode(err), err)
	}
}
