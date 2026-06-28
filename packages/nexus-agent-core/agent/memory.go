package agent

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Memory types — the operator-specific taxonomy. preference + procedure are global
// (they apply on every environment); baseline + entity are env-scoped (a prod
// baseline or named entity is not a dev one).
const (
	MemPreference = "preference" // the operator's habits / defaults (global)
	MemBaseline   = "baseline"   // a confirmed normal range — cost/latency/volume (per-env)
	MemEntity     = "entity"     // a named provider / VK / project / node mapping (per-env)
	MemProcedure  = "procedure"  // a runbook the operator confirmed works (global)
)

// memTypes is the closed set; a remember with any other type is rejected.
var memTypes = map[string]bool{MemPreference: true, MemBaseline: true, MemEntity: true, MemProcedure: true}

// isGlobalType reports whether a type defaults to the global scope.
func isGlobalType(t string) bool { return t == MemPreference || t == MemProcedure }

// MemoryStore is the kernel's durable-memory seam. The CLI binds a file-backed
// impl; the web binds a per-user DB impl constructed bound to a userId (so the
// kernel-facing methods stay userId-free and isolation lives in the impl).
type MemoryStore interface {
	Index() (string, error)
	Recall(name string) (MemoryFact, bool, error)
	Remember(f MemoryFact) error
	Update(name, body string) error
	Forget(name string) (bool, error)
}

// MemoryFact is one durable, cross-session fact the agent has learned.
type MemoryFact struct {
	Name        string // slug — the stable handle recall/update/forget use
	Description string // one-line recall hook (what the index shows)
	Type        string // preference | baseline | entity | procedure
	Body        string // the fact in detail
	Global      bool   // which scope holds it — DERIVED from Type (set by Recall), not an input
}

// Memory is a scope-split, per-fact-file store of durable facts the agent learns —
// modeled on Claude Code's file memory so the CLI gets smarter the more it is used.
// Global facts (preferences, procedures) live under <base>/global; env-specific
// facts (baselines, named entities) under <base>/<env>. Each fact is one markdown
// file with a name/description/type frontmatter header. The agent loads only a
// merged one-line INDEX into each turn's context and recalls a full fact on demand,
// so the per-turn bundle stays small (and the cached system prefix is untouched —
// the index rides the volatile tail). No secrets are stored. Pure stdlib: the
// kernel depends on nothing else.
type Memory struct {
	base    string
	env     string // sanitized env label (the per-env subdirectory name)
	envName string // display label for the index section
}

// OpenMemoryStore opens the store rooted at baseDir for the active env. Global facts
// go under baseDir/global and env facts under baseDir/<env>.
func OpenMemoryStore(baseDir, env string) *Memory {
	return &Memory{base: baseDir, env: sanitizeEnv(env), envName: strings.TrimSpace(env)}
}

func (m *Memory) dir(global bool) string {
	if global {
		return filepath.Join(m.base, "global")
	}
	return filepath.Join(m.base, m.env)
}

// Index renders the merged one-line index (global + the active env), newest-first
// within each scope, that rides each turn's context. Empty when nothing is stored.
// A fact whose file cannot be parsed is skipped, never aborting the load.
func (m *Memory) Index() (string, error) {
	var b strings.Builder
	writeScope := func(label string, global bool) {
		facts := m.list(global)
		if len(facts) == 0 {
			return
		}
		fmt.Fprintf(&b, "### %s\n", label)
		for _, f := range facts {
			desc := f.Description
			if desc == "" {
				desc = f.Body
			}
			fmt.Fprintf(&b, "- %s [%s] — %s\n", f.Name, f.Type, oneLine(desc))
		}
	}
	writeScope("Global", true)
	env := m.envName
	if env == "" {
		env = "Environment"
	}
	writeScope(env, false)
	return strings.TrimRight(b.String(), "\n"), nil
}

// list returns the parsed facts in a scope, sorted by slug ascending. A file that
// is not a parseable fact (wrong extension, missing frontmatter) is skipped.
func (m *Memory) list(global bool) []MemoryFact {
	entries, err := os.ReadDir(m.dir(global))
	if err != nil {
		return nil // missing dir = no facts yet
	}
	type tf struct {
		f    MemoryFact
		name string
	}
	var out []tf
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(m.dir(global), e.Name()))
		if err != nil {
			continue
		}
		f, ok := parseFact(raw)
		if !ok {
			continue
		}
		f.Global = global
		out = append(out, tf{f: f, name: e.Name()})
	}
	// Sort by slug ascending — deterministic and independent of mod time, so the
	// loaded index is byte-stable turn-over-turn (no churn in the volatile tail).
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	facts := make([]MemoryFact, len(out))
	for i := range out {
		facts[i] = out[i].f
	}
	return facts
}

// List returns every stored fact across both scopes — the active env's facts
// followed by the global facts, each with Global set and ordered by slug within a
// scope. It is the full enumeration the sync layer needs to mirror memory across
// devices (and what a `memory ls` would render); the per-turn context still loads
// only the compact Index, not this.
func (m *Memory) List() []MemoryFact {
	return append(m.list(false), m.list(true)...)
}

// Recall reads one fact by name, checking the active env first then global. ok is
// false when no such fact exists.
func (m *Memory) Recall(name string) (MemoryFact, bool, error) {
	slug := slugify(name)
	if slug == "" {
		return MemoryFact{}, false, nil
	}
	for _, global := range []bool{false, true} {
		raw, err := os.ReadFile(filepath.Join(m.dir(global), slug+".md"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return MemoryFact{}, false, fmt.Errorf("read memory %q: %w", slug, err)
		}
		f, ok := parseFact(raw)
		if !ok {
			return MemoryFact{}, false, fmt.Errorf("memory %q is corrupt", slug)
		}
		f.Global = global
		return f, true, nil
	}
	return MemoryFact{}, false, nil
}

// Remember writes a fact, creating its scope dir. A fact with the same slug is
// overwritten (dedup = update-in-place, never a duplicate). Secrets are refused.
func (m *Memory) Remember(f MemoryFact) error {
	if !memTypes[f.Type] {
		return fmt.Errorf("unknown memory type %q (want preference|baseline|entity|procedure)", f.Type)
	}
	f.Name = slugify(f.Name)
	if f.Name == "" {
		return fmt.Errorf("a memory needs a title")
	}
	if strings.TrimSpace(f.Body) == "" {
		return fmt.Errorf("a memory needs a fact body")
	}
	if s := looksLikeSecret(f.Body + " " + f.Description); s != "" {
		return fmt.Errorf("refusing to store what looks like a secret (%s); memory holds durable facts, never credentials", s)
	}
	// Scope is derived from the type: preference/procedure are global, baseline/
	// entity are env-scoped. The taxonomy fixes the scope, so there is no separate
	// override to drift out of sync with it.
	dir := m.dir(isGlobalType(f.Type))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, f.Name+".md"), []byte(renderFact(f)), 0o600)
}

// Update rewrites an existing fact's body, keeping its frontmatter. It errors if no
// fact with that name exists in either scope.
func (m *Memory) Update(name, body string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("update needs a new fact body")
	}
	if s := looksLikeSecret(body); s != "" {
		return fmt.Errorf("refusing to store what looks like a secret (%s)", s)
	}
	f, ok, err := m.Recall(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no memory named %q to update", slugify(name))
	}
	f.Body = body
	return os.WriteFile(filepath.Join(m.dir(f.Global), f.Name+".md"), []byte(renderFact(f)), 0o600)
}

// Forget deletes a fact by name from whichever scope holds it. removed is false
// when there was nothing to forget.
func (m *Memory) Forget(name string) (bool, error) {
	slug := slugify(name)
	if slug == "" {
		return false, nil
	}
	for _, global := range []bool{false, true} {
		path := filepath.Join(m.dir(global), slug+".md")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		if err := os.Remove(path); err != nil {
			return false, fmt.Errorf("forget %q: %w", slug, err)
		}
		return true, nil
	}
	return false, nil
}

// --- frontmatter render / parse (hand-rolled; the kernel imports no YAML) ---

func renderFact(f MemoryFact) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", f.Name)
	fmt.Fprintf(&b, "description: %s\n", oneLine(f.Description))
	fmt.Fprintf(&b, "type: %s\n", f.Type)
	b.WriteString("---\n")
	b.WriteString(strings.TrimSpace(f.Body))
	b.WriteString("\n")
	return b.String()
}

// parseFact reads a fact file: a "---" frontmatter block of key: value lines, then
// the body. ok is false if the frontmatter is missing or has no name.
func parseFact(raw []byte) (MemoryFact, bool) {
	s := string(raw)
	if !strings.HasPrefix(s, "---\n") {
		return MemoryFact{}, false
	}
	rest := s[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return MemoryFact{}, false
	}
	head := rest[:end]
	body := strings.TrimPrefix(rest[end+len("\n---"):], "\n")
	var f MemoryFact
	for _, ln := range strings.Split(head, "\n") {
		k, v, ok := strings.Cut(ln, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "name":
			f.Name = v
		case "description":
			f.Description = v
		case "type":
			f.Type = v
		}
	}
	if f.Name == "" {
		return MemoryFact{}, false
	}
	f.Body = strings.TrimSpace(body)
	return f, true
}

// --- helpers ---

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify turns a title into a filesystem-safe, stable handle. Exported
// because the slug IS a memory fact's identity across the sync ledger: every
// store that canonicalizes a fact name (device store, cloud write-through)
// must use this exact function, or the same fact forks into two ledger
// objects that never converge.
func Slugify(s string) string {
	out := slugRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	return strings.Trim(out, "-")
}

// slugify is the package-internal spelling of Slugify.
func slugify(s string) string { return Slugify(s) }

// sanitizeEnv keeps the env subdirectory name to a safe slug (never empty so the
// path is always well-formed).
func sanitizeEnv(env string) string {
	s := slugify(env)
	if s == "" {
		return "default"
	}
	return s
}

// OneLine collapses whitespace/newlines so a description is a single index
// line. Exported for the same reason as Slugify: it is the canonical payload
// normalization for a memory fact's description/body, and every writer
// (device or cloud) must produce byte-identical output for sync convergence.
func OneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// oneLine is the package-internal spelling of OneLine.
func oneLine(s string) string { return OneLine(s) }

// secretMarkers are conservative credential signals — refusing on these keeps an
// accidental key out of memory without false-positiving on UUIDs / ordinary facts.
var secretMarkers = []string{"nvk_", "sk-", "bearer ", "password", "api_key", "apikey", "secret:", "private key", "-----begin"}

// looksLikeSecret returns the matched marker if text appears to carry a credential,
// else "". Best-effort; the system prompt also instructs the model never to store one.
func looksLikeSecret(text string) string {
	low := strings.ToLower(text)
	for _, mk := range secretMarkers {
		if strings.Contains(low, mk) {
			return mk
		}
	}
	return ""
}

// --- tools: recall / remember / update / forget (auto-tier kernel builtins) ---

type recallTool struct{ m MemoryStore }
type rememberTool struct{ m MemoryStore }
type updateMemoryTool struct{ m MemoryStore }
type forgetTool struct{ m MemoryStore }

func newRecallTool(m MemoryStore) *recallTool             { return &recallTool{m: m} }
func newRememberTool(m MemoryStore) *rememberTool         { return &rememberTool{m: m} }
func newUpdateMemoryTool(m MemoryStore) *updateMemoryTool { return &updateMemoryTool{m: m} }
func newForgetTool(m MemoryStore) *forgetTool             { return &forgetTool{m: m} }

func (r *recallTool) Name() string { return "recall" }
func (r *recallTool) Description() string {
	return "Read the full body of one remembered fact by its name (from the memory index in the context). Use it when an index line looks relevant to the question."
}
func (r *recallTool) Tier() Tier { return TierAuto }
func (r *recallTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
}
func (r *recallTool) Run(_ context.Context, in json.RawMessage) (Result, error) {
	var v struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(in, &v); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}, nil
	}
	f, ok, err := r.m.Recall(v.Name)
	if err != nil {
		return Result{Content: "could not recall: " + err.Error(), IsError: true}, nil
	}
	if !ok {
		return Result{Content: fmt.Sprintf("no remembered fact named %q", slugify(v.Name)), IsError: true}, nil
	}
	return Result{Content: f.Body}, nil
}

func (r *rememberTool) Name() string { return "remember" }
func (r *rememberTool) Description() string {
	return "Save a durable fact so future sessions are smarter. type: preference (operator habit/default, global) | baseline (a confirmed normal cost/latency/volume range, per-env) | entity (a named provider/key/project/node mapping, per-env) | procedure (a runbook that worked, global). title is a short handle; description is the one-line recall hook; fact is the detail. Re-saving the same title updates it in place. NEVER store secrets (keys, tokens, passwords) or transient values a tool can re-fetch."
}
func (r *rememberTool) Tier() Tier { return TierAuto }
func (r *rememberTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"type":{"type":"string","enum":["preference","baseline","entity","procedure"]},"title":{"type":"string"},"fact":{"type":"string"},"description":{"type":"string"}},"required":["type","title","fact"]}`)
}
func (r *rememberTool) Run(_ context.Context, in json.RawMessage) (Result, error) {
	var v struct {
		Type        string `json:"type"`
		Title       string `json:"title"`
		Fact        string `json:"fact"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(in, &v); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}, nil
	}
	title := strings.TrimSpace(v.Title)
	desc := strings.TrimSpace(v.Description)
	// The model sometimes calls remember without an explicit title; derive a stable
	// one from the description (else the fact) so a remember never fails for lacking
	// a handle. Re-saving the same derived title still updates in place via the slug.
	if title == "" {
		if desc != "" {
			title = firstWords(desc, 6)
		} else {
			title = firstWords(v.Fact, 6)
		}
	}
	if desc == "" {
		desc = title
	}
	f := MemoryFact{Name: title, Description: desc, Type: v.Type, Body: v.Fact}
	if err := r.m.Remember(f); err != nil {
		return Result{Content: "could not save: " + err.Error(), IsError: true}, nil
	}
	return Result{Content: fmt.Sprintf("remembered %q", slugify(title))}, nil
}

// firstWords returns up to n whitespace-separated words of s — used to derive a
// fallback memory title when the model omits one.
func firstWords(s string, n int) string {
	fields := strings.Fields(s)
	if len(fields) > n {
		fields = fields[:n]
	}
	return strings.Join(fields, " ")
}

func (u *updateMemoryTool) Name() string { return "update_memory" }
func (u *updateMemoryTool) Description() string {
	return "Replace the body of an existing remembered fact (by name) when what you knew has changed — keeps one fact current instead of accumulating stale duplicates."
}
func (u *updateMemoryTool) Tier() Tier { return TierAuto }
func (u *updateMemoryTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"fact":{"type":"string"}},"required":["name","fact"]}`)
}
func (u *updateMemoryTool) Run(_ context.Context, in json.RawMessage) (Result, error) {
	var v struct {
		Name string `json:"name"`
		Fact string `json:"fact"`
	}
	if err := json.Unmarshal(in, &v); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}, nil
	}
	if err := u.m.Update(v.Name, v.Fact); err != nil {
		return Result{Content: "could not update: " + err.Error(), IsError: true}, nil
	}
	return Result{Content: fmt.Sprintf("updated %q", slugify(v.Name))}, nil
}

func (f *forgetTool) Name() string { return "forget" }
func (f *forgetTool) Description() string {
	return "Delete a remembered fact by name when it is wrong or stale."
}
func (f *forgetTool) Tier() Tier { return TierAuto }
func (f *forgetTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
}
func (f *forgetTool) Run(_ context.Context, in json.RawMessage) (Result, error) {
	var v struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(in, &v); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}, nil
	}
	removed, err := f.m.Forget(v.Name)
	if err != nil {
		return Result{Content: "could not forget: " + err.Error(), IsError: true}, nil
	}
	if !removed {
		return Result{Content: fmt.Sprintf("no remembered fact named %q", slugify(v.Name))}, nil
	}
	return Result{Content: fmt.Sprintf("forgot %q", slugify(v.Name))}, nil
}
