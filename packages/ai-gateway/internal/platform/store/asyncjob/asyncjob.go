// Package asyncjob is the gateway-owned async-job correlation store over the
// gateway_async_job table — the ONE runtime-writable table the AI Gateway
// owns (the parent store package is otherwise read-only). One row per
// provider-side async job (video generation today; batch later), written
// synchronously at submit because a client may poll immediately after submit:
// the MQ→Hub path is async-racy and Redis is ephemeral against hours-long
// jobs. The row is a correlation/authz record, never a payload store — no
// prompt text, no artifact bytes.
//
// Store is the narrow interface handlers are tested against. The
// implementation is hand-written SQL over the existing pgx pool; raw-SQL
// gate rationale: the Bun storage leg is not present in this module graph,
// so the interface seam IS the migration boundary — when a Bun-backed
// implementation becomes available it swaps in behind Store with zero
// handler change.
package asyncjob

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// Job statuses — a cache of the LAST OBSERVED provider state. queued and
// in_progress are the non-terminal set the render bound counts; expired is
// gateway-invented by retention (the provider never reports it).
const (
	StatusQueued     = "queued"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
	StatusCanceled   = "canceled"
	StatusExpired    = "expired"
)

// Sentinel errors handlers map to HTTP statuses.
var (
	// ErrConflict: a row with the same (provider_id, id) already exists. The
	// insert is a plain INSERT that fails and surfaces — NEVER an upsert,
	// which would let a provider response echoing another job's id rebind
	// virtual_key_id and hijack the correlation. Handlers surface it as 502
	// with the job flagged orphaned.
	ErrConflict = errors.New("asyncjob: a job with this provider-scoped id already exists")
	// ErrNotFound: no row for (provider_id, id). Handlers surface it as the
	// 404 non-disclosure (never forwarded upstream).
	ErrNotFound = errors.New("asyncjob: job not found")
	// ErrInvalidStatus: a write carried a status outside the closed
	// vocabulary. The store is the single choke point every provider leg's
	// codec flows through — an unmapped provider state must fail loud here,
	// not corrupt the lifecycle state machine (an out-of-enum status would
	// dodge the render-bound count and the retention sweep's arms).
	ErrInvalidStatus = errors.New("asyncjob: status is outside the job-status vocabulary")
	// ErrAmbiguous: an owned lookup by client-supplied id matched MORE than
	// one row — the same VK holds jobs on two providers that issued the same
	// id string. The gateway cannot know which provider the client means, and
	// silently picking one could relay a poll/delete to the wrong provider
	// account; handlers surface the collision loud (502) rather than guess.
	ErrAmbiguous = errors.New("asyncjob: id matches jobs on more than one provider for this key")
)

// validStatus reports membership in the closed status vocabulary.
func validStatus(s string) bool {
	switch s {
	case StatusQueued, StatusInProgress, StatusCompleted, StatusFailed, StatusCanceled, StatusExpired:
		return true
	}
	return false
}

// Job is one gateway_async_job row.
type Job struct {
	ProviderID     string
	ID             string // the provider's job id (Veo: the veo_-encoded canonical form)
	EndpointType   string
	VirtualKeyID   string // stable VK uuid — the authz binding
	OrganizationID string
	ProjectID      string // "" when absent
	CredentialID   string // submit-time credential pin; "" when absent
	ModelID        string
	RequestedUnits float64 // requested seconds (the estimate-as-floor basis)
	Status         string
	CostReconciled bool
	SubmitTraceID  string
	CreatedAt      time.Time
	CompletedAt    *time.Time
	CanceledAt     *time.Time
	ExpiresAt      *time.Time
}

// Terminal reports whether s is a terminal job status (no further provider
// progress possible from the gateway's point of view).
func Terminal(s string) bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCanceled, StatusExpired:
		return true
	}
	return false
}

// Observation is the provider state a poll observed, applied to the row's
// last-observed cache by MarkObserved.
type Observation struct {
	Status      string
	CompletedAt *time.Time
	ExpiresAt   *time.Time
}

// Store is the async-job correlation surface the ServeVideo* handlers (and
// the retention sweep) consume. Handlers are tested against this interface.
type Store interface {
	// Insert writes the job row at submit. A (provider_id, id) conflict
	// returns ErrConflict — plain INSERT semantics, never upsert. An
	// out-of-vocabulary status returns ErrInvalidStatus.
	Insert(ctx context.Context, j Job) error
	// GetByID loads the row for the provider-scoped id; ErrNotFound when no
	// such row exists.
	GetByID(ctx context.Context, providerID, id string) (Job, error)
	// GetOwned is the FOLLOW-UP entry lookup: the poll/content/delete client
	// supplies only the job id, so the row is found by (id, virtual_key_id,
	// endpoint_type) and every downstream operation then uses the returned
	// row's provider_id (the provider-scoped identity, D-V4). The
	// virtual_key_id predicate is the authz binding IN the query: a foreign
	// VK's job matches zero rows, so "someone else's job" and "no such job"
	// collapse into the same ErrNotFound → the handler's 404 non-disclosure
	// cannot distinguish them even by accident. ErrAmbiguous when two
	// providers issued the same id string to this VK (never guess a
	// provider).
	GetOwned(ctx context.Context, id, virtualKeyID, endpointType string) (Job, error)
	// MarkObserved updates the row's last-observed provider state after a
	// poll relay. ErrNotFound when the row vanished; ErrInvalidStatus when
	// the codec let an unmapped provider state through.
	MarkObserved(ctx context.Context, providerID, id string, obs Observation) error
	// ReconcileOnce atomically claims the single cost reconciliation for the
	// job: it returns true for AT MOST one caller (the first observer of a
	// terminal provider state); every later call returns false. The claim
	// guards Engine.Reconcile so live quota never double-counts a job. The
	// claim-then-act order means a crash between the committed claim and the
	// live Reconcile loses that live debit permanently — the conservative
	// direction (the inverse order double-debits under concurrent polls),
	// and the persistent counters self-heal via the submit row's
	// estimate-as-floor backfill.
	ReconcileOnce(ctx context.Context, providerID, id string) (bool, error)
	// MarkCanceled records a client cancel (relayed DELETE, or best-effort
	// local mark on legs without a real provider delete). virtualKeyID is a
	// defense-in-depth guard on the one client-triggered mutation: a row
	// owned by a different VK matches zero rows → ErrNotFound, so a
	// forgotten handler authz check structurally collapses into the same
	// 404 non-disclosure instead of a cross-VK cancel.
	MarkCanceled(ctx context.Context, providerID, id, virtualKeyID string) error
	// CountNonTerminal counts the VK's rows NOT in a terminal status — the
	// render bound the submit admission checks (the gencaps concurrency row
	// bounds HTTP requests, not provider renders). Formulated as
	// NOT-IN-terminal so any status outside the vocabulary (belt-and-braces
	// vs the write-side validation) counts AGAINST the cap — errors fail
	// toward bounding spend.
	CountNonTerminal(ctx context.Context, virtualKeyID string) (int, error)
	// MarkExpired sets status=expired on terminal rows created before
	// terminalCutoff and non-terminal rows created before nonTerminalCutoff,
	// returning how many rows were marked. Mark-only, never delete: our
	// status is a last-observed cache (a "stale non-terminal" row may be
	// provider-complete) and the attribution/audit record must survive; an
	// expired row serves 410 Gone.
	MarkExpired(ctx context.Context, terminalCutoff, nonTerminalCutoff time.Time) (int64, error)
}

// DB implements Store over the gateway's pgx pool.
type DB struct {
	pool store.PgxPool
}

// New wraps the pool. Callers hold the same pool the read-only store uses.
func New(pool store.PgxPool) *DB {
	return &DB{pool: pool}
}

// jobColumns single-sources the SELECT list; scanJob's positional Scan below
// must mirror this order (a live-DB round-trip — the gateway smoke — is what
// verifies the pairing; pgxmock cannot).
const jobColumns = `provider_id, id, endpoint_type, virtual_key_id, organization_id,
	COALESCE(project_id, ''), COALESCE(credential_id, ''), model_id, requested_units,
	status, cost_reconciled, submit_trace_id, created_at, completed_at, canceled_at, expires_at`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ProviderID, &j.ID, &j.EndpointType, &j.VirtualKeyID, &j.OrganizationID,
		&j.ProjectID, &j.CredentialID, &j.ModelID, &j.RequestedUnits,
		&j.Status, &j.CostReconciled, &j.SubmitTraceID, &j.CreatedAt, &j.CompletedAt, &j.CanceledAt, &j.ExpiresAt)
	return j, err
}

// nullable maps "" to SQL NULL for the optional text columns.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Insert implements Store.
func (db *DB) Insert(ctx context.Context, j Job) error {
	if !validStatus(j.Status) {
		return fmt.Errorf("%w: %q", ErrInvalidStatus, j.Status)
	}
	_, err := db.pool.Exec(ctx, `
		INSERT INTO gateway_async_job (
			provider_id, id, endpoint_type, virtual_key_id, organization_id,
			project_id, credential_id, model_id, requested_units, status,
			submit_trace_id, expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		j.ProviderID, j.ID, j.EndpointType, j.VirtualKeyID, j.OrganizationID,
		nullable(j.ProjectID), nullable(j.CredentialID), j.ModelID, j.RequestedUnits, j.Status,
		j.SubmitTraceID, j.ExpiresAt)
	if err != nil {
		// Only the composite-PK unique violation means "this provider-scoped
		// job already exists"; a 23505 from any future unique index must not
		// masquerade as the hijack-guard conflict.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "gateway_async_job_pkey" {
			return ErrConflict
		}
		return fmt.Errorf("asyncjob insert: %w", err)
	}
	return nil
}

// GetByID implements Store.
func (db *DB) GetByID(ctx context.Context, providerID, id string) (Job, error) {
	j, err := scanJob(db.pool.QueryRow(ctx,
		`SELECT `+jobColumns+` FROM gateway_async_job WHERE provider_id = $1 AND id = $2`,
		providerID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("asyncjob get: %w", err)
	}
	return j, nil
}

// GetOwned implements Store. LIMIT 2 caps the scan: one row is the answer,
// two proves the cross-provider id collision (ErrAmbiguous) — the count is
// all that matters past the first row, so no more are fetched. The
// (virtual_key_id, status) index serves the virtual_key_id equality; a VK's
// job rows are bounded small (render cap + retention), so the residual id
// filter is a handful of rows.
func (db *DB) GetOwned(ctx context.Context, id, virtualKeyID, endpointType string) (Job, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT `+jobColumns+` FROM gateway_async_job
		WHERE id = $1 AND virtual_key_id = $2 AND endpoint_type = $3 LIMIT 2`,
		id, virtualKeyID, endpointType)
	if err != nil {
		return Job{}, fmt.Errorf("asyncjob get owned: %w", err)
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		j, scanErr := scanJob(rows)
		if scanErr != nil {
			return Job{}, fmt.Errorf("asyncjob get owned scan: %w", scanErr)
		}
		jobs = append(jobs, j)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return Job{}, fmt.Errorf("asyncjob get owned rows: %w", rowsErr)
	}
	switch len(jobs) {
	case 0:
		return Job{}, ErrNotFound
	case 1:
		return jobs[0], nil
	default:
		return Job{}, ErrAmbiguous
	}
}

// MarkObserved implements Store.
func (db *DB) MarkObserved(ctx context.Context, providerID, id string, obs Observation) error {
	if !validStatus(obs.Status) {
		return fmt.Errorf("%w: %q", ErrInvalidStatus, obs.Status)
	}
	tag, err := db.pool.Exec(ctx, `
		UPDATE gateway_async_job
		SET status = $3,
		    completed_at = COALESCE($4, completed_at),
		    expires_at   = COALESCE($5, expires_at)
		WHERE provider_id = $1 AND id = $2`,
		providerID, id, obs.Status, obs.CompletedAt, obs.ExpiresAt)
	if err != nil {
		return fmt.Errorf("asyncjob mark observed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ReconcileOnce implements Store. The WHERE cost_reconciled = false predicate
// makes the claim atomic under concurrent polls: PostgreSQL row-locks the
// UPDATE, so at most one transaction observes false and flips it; every
// other caller matches zero rows.
func (db *DB) ReconcileOnce(ctx context.Context, providerID, id string) (bool, error) {
	tag, err := db.pool.Exec(ctx, `
		UPDATE gateway_async_job
		SET cost_reconciled = true
		WHERE provider_id = $1 AND id = $2 AND cost_reconciled = false`,
		providerID, id)
	if err != nil {
		return false, fmt.Errorf("asyncjob reconcile once: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkCanceled implements Store. The virtual_key_id predicate is the
// defense-in-depth guard: a foreign VK's row matches zero rows → ErrNotFound.
// The CASE keeps an already-terminal status (a delete after completion must
// not rewrite the record that the render completed and was accounted) while
// still stamping canceled_at — the truthful "the client asked for deletion at
// this time" mark.
func (db *DB) MarkCanceled(ctx context.Context, providerID, id, virtualKeyID string) error {
	tag, err := db.pool.Exec(ctx, `
		UPDATE gateway_async_job
		SET status = CASE WHEN status IN ($5, $6, $7) THEN status ELSE $3 END,
		    canceled_at = now()
		WHERE provider_id = $1 AND id = $2 AND virtual_key_id = $4`,
		providerID, id, StatusCanceled, virtualKeyID,
		StatusCompleted, StatusFailed, StatusExpired)
	if err != nil {
		return fmt.Errorf("asyncjob mark canceled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountNonTerminal implements Store. One indexed query on
// (virtual_key_id, status). NOT-IN-terminal (rather than IN-non-terminal) so
// an out-of-vocabulary status — impossible via this package's validated
// writes, but the DB carries no CHECK — counts against the cap instead of
// silently freeing a render slot.
func (db *DB) CountNonTerminal(ctx context.Context, virtualKeyID string) (int, error) {
	var n int
	err := db.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM gateway_async_job
		WHERE virtual_key_id = $1 AND status NOT IN ($2, $3, $4, $5)`,
		virtualKeyID, StatusCompleted, StatusFailed, StatusCanceled, StatusExpired).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("asyncjob count non-terminal: %w", err)
	}
	return n, nil
}

// MarkExpired implements Store. status <> expired keeps the sweep idempotent
// (already-expired rows are not re-counted run over run). The non-terminal
// arm is NOT-IN-terminal so an out-of-vocabulary status still gets swept at
// the (shorter) non-terminal cutoff instead of living forever.
func (db *DB) MarkExpired(ctx context.Context, terminalCutoff, nonTerminalCutoff time.Time) (int64, error) {
	tag, err := db.pool.Exec(ctx, `
		UPDATE gateway_async_job
		SET status = $1
		WHERE status <> $1
		  AND (
		        (status IN ($2, $3, $4) AND created_at < $5)
		     OR (status NOT IN ($2, $3, $4) AND created_at < $6)
		  )`,
		StatusExpired,
		StatusCompleted, StatusFailed, StatusCanceled, terminalCutoff,
		nonTerminalCutoff)
	if err != nil {
		return 0, fmt.Errorf("asyncjob mark expired: %w", err)
	}
	return tag.RowsAffected(), nil
}
