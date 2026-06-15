package assistant

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
)

// maxFileBytes caps a single sandbox file. The assistant generates text artifacts
// (reports, configs) for the user to download — not bulk uploads — so a modest cap
// keeps the shared spill backend bounded.
const maxFileBytes = 5 << 20 // 5 MiB

// maxFileNameLen bounds the model-supplied file name (it is metadata, not a path).
const maxFileNameLen = 255

// maxUserFileBytes caps the TOTAL sandbox footprint of one user across all their
// sessions. Without it a user could accumulate unbounded artifacts in
// the shared spill backend even though each file is individually small. A fixed,
// generous default (no per-deployment knob, per less-is-more) — the assistant
// produces text reports, not media — and it degrades safely: over-quota writes are
// rejected with a clear tool error; deleting a session reclaims that session's files
// (rows + spill content), so the quota is recoverable.
//
// This is a COURTESY soft-cap, not a hard security boundary: the SUM-then-INSERT check
// is not serialized, so two write_file calls in the SAME turn (TierAuto → parallel) can
// each observe headroom and both insert, overshooting by up to (N-1)×maxFileBytes. The
// overshoot is bounded and self-heals on session delete; a hard guarantee would need a
// per-user advisory lock / SERIALIZABLE, which is disproportionate for a soft cap.
const maxUserFileBytes = 50 << 20 // 50 MiB per user

// fileMeta is the user-facing descriptor of a sandbox file.
type fileMeta struct {
	ID          string
	Name        string
	ContentType string
	Size        int
}

// fileStore is the per-user/session file-sandbox seam the write_file/read_file
// tools use. Like dbStore it is bound to one authenticated userId; content
// lives in the shared spillstore, metadata + ref in the AssistantFile table.
type fileStore interface {
	Write(name, content, contentType string) (fileMeta, error)
	ReadByName(name string) (string, error)
}

// errFileExpired signals that a file's metadata row survives but its content was
// reaped under the shared spill retention — callers degrade (410 / "expired")
// rather than report a generic failure, matching the session-transcript contract.
var errFileExpired = errors.New("file content has expired")

// assistantFilesPath is the download route prefix the write_file tool reports in its
// result AND the handler parses to emit the structured `file` SSE event. Declared once
// so the producer (write_file output) and the consumer (fileIDFromToolOutput) cannot
// drift. It MUST match the registered download route (`/api/admin` group +
// `/assistant/files/:id` in routes.go), which is a separate literal — keep them in sync.
const assistantFilesPath = "/api/admin/assistant/files/"

var fileIDRe = regexp.MustCompile(regexp.QuoteMeta(assistantFilesPath) + `([a-f0-9]+)`)

// fileIDFromToolOutput extracts the sandbox file id from a successful write_file tool
// result. The handler uses it to emit a structured `file` SSE event so the download
// button no longer depends on the MODEL echoing the URL into its prose (the prior
// fragility) — the id comes from the tool's own server-controlled output. Returns
// false when no file path is present (e.g. a non-write_file tool, or an error result).
func fileIDFromToolOutput(s string) (string, bool) {
	m := fileIDRe.FindStringSubmatch(s)
	if len(m) != 2 {
		return "", false
	}
	return m[1], true
}

type webFileStore struct {
	pool      pgxPool
	spill     spillstore.SpillStore
	userID    string
	sessionID string
	ctx       context.Context
}

func newWebFileStore(ctx context.Context, pool pgxPool, spill spillstore.SpillStore, userID, sessionID string) *webFileStore {
	return &webFileStore{pool: pool, spill: spill, userID: userID, sessionID: sessionID, ctx: ctx}
}

func newFileID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (f *webFileStore) Write(name, content, contentType string) (fileMeta, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return fileMeta{}, fmt.Errorf("file name is required")
	}
	// Cap the name length: it is model-supplied and rendered first in the tool's result
	// line, so an absurdly long name could push the download path past the tool-output
	// cap and silently drop the structured `file` event + the history scrape.
	if len(name) > maxFileNameLen {
		return fileMeta{}, fmt.Errorf("file name too long (max %d characters)", maxFileNameLen)
	}
	if len(content) > maxFileBytes {
		return fileMeta{}, fmt.Errorf("file exceeds the %d-byte sandbox limit", maxFileBytes)
	}
	// Per-user total-storage quota. Checked BEFORE spilling content so an
	// over-quota write leaves no orphaned spill object. SUM is scoped to this caller's
	// userId across all their sessions.
	var used int64
	if err := f.pool.QueryRow(f.ctx,
		`SELECT COALESCE(SUM(size), 0) FROM "AssistantFile" WHERE "userId" = $1`,
		f.userID).Scan(&used); err != nil {
		return fileMeta{}, fmt.Errorf("check storage quota: %w", err)
	}
	if used+int64(len(content)) > maxUserFileBytes {
		return fileMeta{}, fmt.Errorf("storage quota exceeded: the %d-byte per-user sandbox limit would be exceeded (currently using %d bytes) — delete a session to free space", maxUserFileBytes, used)
	}
	if strings.TrimSpace(contentType) == "" {
		contentType = "text/plain; charset=utf-8"
	}
	id := newFileID()
	ref, err := f.spill.Put(f.ctx, bytes.NewReader([]byte(content)), int64(len(content)), spillstore.PutOptions{
		EventID:     f.sessionID + "/" + id,
		Direction:   "file",
		ContentType: contentType,
	})
	if err != nil {
		return fileMeta{}, fmt.Errorf("spill file: %w", err)
	}
	refJSON, err := json.Marshal(ref)
	if err != nil {
		return fileMeta{}, fmt.Errorf("marshal spill ref: %w", err)
	}
	_, err = f.pool.Exec(f.ctx, `
		INSERT INTO "AssistantFile" (id, "userId", "sessionId", name, size, "contentType", "spillRef", "createdAt")
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())`,
		id, f.userID, f.sessionID, name, len(content), contentType, refJSON)
	if err != nil {
		return fileMeta{}, fmt.Errorf("save file: %w", err)
	}
	return fileMeta{ID: id, Name: name, ContentType: contentType, Size: len(content)}, nil
}

func (f *webFileStore) ReadByName(name string) (string, error) {
	var refJSON []byte
	err := f.pool.QueryRow(f.ctx, `
		SELECT "spillRef" FROM "AssistantFile"
		 WHERE "userId" = $1 AND "sessionId" = $2 AND name = $3
		 ORDER BY "createdAt" DESC LIMIT 1`,
		f.userID, f.sessionID, strings.TrimSpace(name)).Scan(&refJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("no file named %q in this session", name)
	}
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	data, err := f.fetch(refJSON)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Get streams one of the caller's files by id (owner re-checked via WHERE "userId").
// Used by the download endpoint — sessionID is not part of the predicate so a file
// is downloadable across the user's own sessions, but never cross-user.
func (f *webFileStore) Get(id string) (io.ReadCloser, fileMeta, error) {
	var refJSON []byte
	m := fileMeta{ID: id}
	err := f.pool.QueryRow(f.ctx,
		`SELECT name, "contentType", size, "spillRef" FROM "AssistantFile" WHERE id = $1 AND "userId" = $2`,
		id, f.userID).Scan(&m.Name, &m.ContentType, &m.Size, &refJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fileMeta{}, fmt.Errorf("file %s not found", id)
	}
	if err != nil {
		return nil, fileMeta{}, fmt.Errorf("get file: %w", err)
	}
	var ref audit.SpillRef
	if err := json.Unmarshal(refJSON, &ref); err != nil {
		return nil, fileMeta{}, fmt.Errorf("decode spill ref: %w", err)
	}
	rc, err := f.spill.Get(f.ctx, ref)
	if errors.Is(err, spillstore.ErrNotFound) {
		return nil, fileMeta{}, errFileExpired
	}
	if err != nil {
		return nil, fileMeta{}, fmt.Errorf("fetch file: %w", err)
	}
	return rc, m, nil
}

func (f *webFileStore) fetch(refJSON []byte) ([]byte, error) {
	var ref audit.SpillRef
	if err := json.Unmarshal(refJSON, &ref); err != nil {
		return nil, fmt.Errorf("decode spill ref: %w", err)
	}
	rc, err := f.spill.Get(f.ctx, ref)
	if errors.Is(err, spillstore.ErrNotFound) {
		return nil, errFileExpired
	}
	if err != nil {
		return nil, fmt.Errorf("fetch file: %w", err)
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

var _ fileStore = (*webFileStore)(nil)

// write_file / read_file sandbox tools. TierAuto: they only touch the caller's own
// isolated sandbox (no privileged action), so they need no confirm gate. These are
// distinct from the CLI's local-filesystem system tools (never enabled on web).

type writeFileTool struct{ fs fileStore }

func (writeFileTool) Name() string { return "write_file" }
func (writeFileTool) Description() string {
	return "Write a text file to your private, per-session sandbox; the user can download it. Args: name, content, optional contentType."
}
func (writeFileTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"file name shown to the user"},"content":{"type":"string"},"contentType":{"type":"string"}},"required":["name","content"]}`)
}
func (writeFileTool) Tier() agent.Tier { return agent.TierAuto }
func (t writeFileTool) Run(_ context.Context, input json.RawMessage) (agent.Result, error) {
	var a struct {
		Name        string `json:"name"`
		Content     string `json:"content"`
		ContentType string `json:"contentType"`
	}
	if err := json.Unmarshal(input, &a); err != nil {
		return agent.Result{Content: "invalid input: " + err.Error(), IsError: true}, nil //nolint:nilerr // tool failures are surfaced to the LLM via IsError, not as a Go error (which would abort the agent loop)
	}
	m, err := t.fs.Write(a.Name, a.Content, a.ContentType)
	if err != nil {
		return agent.Result{Content: err.Error(), IsError: true}, nil //nolint:nilerr // tool failures are surfaced to the LLM via IsError, not as a Go error (which would abort the agent loop)
	}
	return agent.Result{Content: fmt.Sprintf("wrote %q (%d bytes); download at %s%s", m.Name, m.Size, assistantFilesPath, m.ID)}, nil
}

type readFileTool struct{ fs fileStore }

func (readFileTool) Name() string { return "read_file" }
func (readFileTool) Description() string {
	return "Read back a file you previously wrote to your sandbox this session. Arg: name."
}
func (readFileTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
}
func (readFileTool) Tier() agent.Tier { return agent.TierAuto }
func (t readFileTool) Run(_ context.Context, input json.RawMessage) (agent.Result, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &a); err != nil {
		return agent.Result{Content: "invalid input: " + err.Error(), IsError: true}, nil //nolint:nilerr // tool failures are surfaced to the LLM via IsError, not as a Go error (which would abort the agent loop)
	}
	content, err := t.fs.ReadByName(a.Name)
	if err != nil {
		return agent.Result{Content: err.Error(), IsError: true}, nil //nolint:nilerr // tool failures are surfaced to the LLM via IsError, not as a Go error (which would abort the agent loop)
	}
	return agent.Result{Content: content}, nil
}
