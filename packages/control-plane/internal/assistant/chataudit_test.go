package assistant

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	pgxmock "github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hashchain"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// rowT is the fixed row timestamp the widened Load mocks return.
var rowT = time.Now().UTC()

// mkChatEvent builds one authentic chain entry exactly the way appendChatEvent
// writes it, so verification tests run against real chains and precise tamper
// variants.
func mkChatEvent(t *testing.T, prev *string, seq int, kind, origin string, msgCount int, digest string) chatChainRow {
	t.Helper()
	envBytes, err := json.Marshal(chatChainEnvelope{Seq: seq, Kind: kind, Origin: origin, MsgCount: msgCount, ContentDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	hashInput, err := hashchain.Canonicalize(envBytes)
	if err != nil {
		t.Fatal(err)
	}
	return chatChainRow{Seq: seq, Kind: kind, ContentDigest: digest,
		PrevHash: prev, Hash: hashchain.ChainHash(prev, hashInput), HashInput: hashInput}
}

func chatEventQueryRows(evs ...chatChainRow) *pgxmock.Rows {
	rows := pgxmock.NewRows([]string{"seq", "kind", "contentDigest", "prevHash", "hash", "hashInput"})
	for _, e := range evs {
		rows.AddRow(e.Seq, e.Kind, e.ContentDigest, e.PrevHash, e.Hash, e.HashInput)
	}
	return rows
}

// chatVerifyFixture spills a transcript, builds its two-revision chain, and
// returns the store + the chain (head digest = the spilled bytes' SHA-256).
func chatVerifyFixture(t *testing.T, mock pgxmock.PgxPoolIface) (*dbStore, []chatChainRow, []byte) {
	t.Helper()
	msgs := []agent.Message{agent.TextMessage(agent.RoleUser, "hello")}
	data, _ := json.Marshal(msgs)
	spill := &fakeSpill{objs: map[string][]byte{"s1:transcript": data}}
	s := newDBStore(context.Background(), mock, spill, "alice")
	e1 := mkChatEvent(t, nil, 1, chatKindRevision, chatOriginWeb, 1, audit.SHA256Hex([]byte("older")))
	e2 := mkChatEvent(t, &e1.Hash, 2, chatKindRevision, chatOriginWeb, 1, audit.SHA256Hex(data))
	refJSON, _ := json.Marshal(audit.SpillRef{Backend: "fake", Key: "s1:transcript"})
	return s, []chatChainRow{e1, e2}, refJSON
}

func expectSessionAnchor(mock pgxmock.PgxPoolIface, refJSON []byte, chainSeq int, chainHead string) {
	mock.ExpectQuery(`SELECT "spillRef","chainSeq","chainHead" FROM "AssistantSession"`).
		WithArgs("s1", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"spillRef", "chainSeq", "chainHead"}).AddRow(refJSON, chainSeq, chainHead))
}

func expectChatEvents(mock pgxmock.PgxPoolIface, evs ...chatChainRow) {
	mock.ExpectQuery(`SELECT "seq","kind","contentDigest","prevHash","hash","hashInput"`).
		WithArgs("s1", "alice").
		WillReturnRows(chatEventQueryRows(evs...))
}

// TestAppendChatEvent_ChainAdvancesAndLinks pins the chain-write contract: the
// new entry's seq is head+1, its prevHash is the head's hash, and its hash
// recomputes from (prevHash, canonical envelope).
func TestAppendChatEvent_ChainAdvancesAndLinks(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	e1 := mkChatEvent(t, nil, 1, chatKindRevision, chatOriginWeb, 1, "d1")
	want := mkChatEvent(t, &e1.Hash, 2, chatKindRevision, chatOriginWeb, 3, "d2")

	mock.ExpectQuery(`SELECT "seq","hash" FROM "AssistantChatEvent"`).
		WithArgs("s1", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"seq", "hash"}).AddRow(1, &e1.Hash))
	mock.ExpectExec(`INSERT INTO "AssistantChatEvent"`).
		WithArgs("s1", "alice", 2, chatKindRevision, chatOriginWeb, 3, "d2",
			&e1.Hash, want.Hash, want.HashInput).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	seq, hash, err := appendChatEvent(context.Background(), mock, "alice", "s1", chatKindRevision, 3, "d2")
	if err != nil || seq != 2 || hash != want.Hash {
		t.Fatalf("append = (%d, %q, %v); want seq 2 linked to the head", seq, hash, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the appended entry did not link to the chain head: %v", err)
	}
}

// TestAppendChatEvent_RetriesOnSeqCollision: losing the head CAS re-reads and
// converges instead of failing or forking the chain.
func TestAppendChatEvent_RetriesOnSeqCollision(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT "seq","hash" FROM "AssistantChatEvent"`).
		WithArgs("s1", "alice").WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO "AssistantChatEvent"`).
		WithArgs("s1", "alice", 1, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: "23505"})
	e1 := mkChatEvent(t, nil, 1, chatKindRevision, "dev-b", 1, "other")
	mock.ExpectQuery(`SELECT "seq","hash" FROM "AssistantChatEvent"`).
		WithArgs("s1", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"seq", "hash"}).AddRow(1, &e1.Hash))
	mock.ExpectExec(`INSERT INTO "AssistantChatEvent"`).
		WithArgs("s1", "alice", 2, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	seq, _, err := appendChatEvent(context.Background(), mock, "alice", "s1", chatKindRevision, 2, "d")
	if err != nil || seq != 2 {
		t.Fatalf("append after collision = (%d, %v); want seq 2", seq, err)
	}
}

func TestVerifySessionChain_Verified(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	s, chain, refJSON := chatVerifyFixture(t, mock)
	expectSessionAnchor(mock, refJSON, 2, chain[1].Hash)
	expectChatEvents(mock, chain...)
	got := s.verifySessionChain("s1")
	if got.Status != chatIntegrityVerified {
		t.Fatalf("verdict = %+v, want verified", got)
	}
}

func TestVerifySessionChain_Unchained(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	s, _, refJSON := chatVerifyFixture(t, mock)
	expectSessionAnchor(mock, refJSON, 0, "")
	expectChatEvents(mock)
	got := s.verifySessionChain("s1")
	if got.Status != chatIntegrityUnchained {
		t.Fatalf("verdict = %+v, want unchained (pre-chain session)", got)
	}
}

// TestVerifySessionChain_TamperedTranscriptDetected: an out-of-band edit of
// the stored transcript blob is detected by name — its digest no longer
// matches the digest the chain head pinned.
func TestVerifySessionChain_TamperedTranscriptDetected(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	s, chain, refJSON := chatVerifyFixture(t, mock)
	tampered, _ := json.Marshal([]agent.Message{agent.TextMessage(agent.RoleUser, "REWRITTEN HISTORY")})
	s.spill.(*fakeSpill).objs["s1:transcript"] = tampered
	expectSessionAnchor(mock, refJSON, 2, chain[1].Hash)
	expectChatEvents(mock, chain...)
	got := s.verifySessionChain("s1")
	if got.Status != chatIntegrityContentMismatch || !strings.Contains(got.Detail, "does not match its chained attestation") {
		t.Fatalf("verdict = %+v, want content_mismatch naming the attestation", got)
	}
}

// TestVerifySessionChain_TamperedEntryDetected: a modified chain row (its
// hash-covered envelope no longer recomputes) breaks the chain by name.
func TestVerifySessionChain_TamperedEntryDetected(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	s, chain, refJSON := chatVerifyFixture(t, mock)
	forged := mkChatEvent(t, nil, 1, chatKindRevision, chatOriginWeb, 1, audit.SHA256Hex([]byte("forged")))
	forged.Hash = chain[0].Hash // stored hash kept; envelope no longer matches
	expectSessionAnchor(mock, refJSON, 2, chain[1].Hash)
	expectChatEvents(mock, forged, chain[1])
	got := s.verifySessionChain("s1")
	if got.Status != chatIntegrityChainBroken || !strings.Contains(got.Detail, "hash mismatch at seq 1") {
		t.Fatalf("verdict = %+v, want chain_broken naming seq 1", got)
	}
}

// TestVerifySessionChain_TruncatedTailDetected: deleting the newest chain rows
// while the session anchor still records them breaks the chain by name — this
// is what makes a rollback-to-older-transcript visible.
func TestVerifySessionChain_TruncatedTailDetected(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	s, chain, refJSON := chatVerifyFixture(t, mock)
	expectSessionAnchor(mock, refJSON, 2, chain[1].Hash)
	expectChatEvents(mock, chain[0]) // tail (seq 2) deleted
	got := s.verifySessionChain("s1")
	if got.Status != chatIntegrityChainBroken || !strings.Contains(got.Detail, "tail truncated") {
		t.Fatalf("verdict = %+v, want chain_broken naming the truncated tail", got)
	}
}

// TestVerifySessionChain_AnchorHeadMismatch: a head row whose hash disagrees
// with the session anchor at the same seq breaks by name.
func TestVerifySessionChain_AnchorHeadMismatch(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	s, chain, refJSON := chatVerifyFixture(t, mock)
	expectSessionAnchor(mock, refJSON, 2, "not-the-head-hash")
	expectChatEvents(mock, chain...)
	got := s.verifySessionChain("s1")
	if got.Status != chatIntegrityChainBroken || !strings.Contains(got.Detail, "anchor disagrees") {
		t.Fatalf("verdict = %+v, want chain_broken naming the anchor", got)
	}
}

// TestVerifySessionChain_SweptTranscriptStaysVerified: spill content expiring
// under retention keeps the chain green with the expiry named — absence of
// sweepable content is not tampering (the workflow-export precedent).
func TestVerifySessionChain_SweptTranscriptStaysVerified(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	s, chain, refJSON := chatVerifyFixture(t, mock)
	delete(s.spill.(*fakeSpill).objs, "s1:transcript")
	expectSessionAnchor(mock, refJSON, 2, chain[1].Hash)
	expectChatEvents(mock, chain...)
	got := s.verifySessionChain("s1")
	if got.Status != chatIntegrityVerified || !strings.Contains(got.Detail, "expired under retention") {
		t.Fatalf("verdict = %+v, want verified with the expiry named", got)
	}
}

// TestVerifySessionChain_TombstoneHeadWithLiveRow: a chain ending in a
// deletion while the session still serves content is a named mismatch.
func TestVerifySessionChain_TombstoneHeadWithLiveRow(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	s, chain, refJSON := chatVerifyFixture(t, mock)
	tomb := mkChatEvent(t, &chain[0].Hash, 2, chatKindTombstone, chatOriginWeb, 0, "")
	expectSessionAnchor(mock, refJSON, 2, tomb.Hash)
	expectChatEvents(mock, chain[0], tomb)
	got := s.verifySessionChain("s1")
	if got.Status != chatIntegrityContentMismatch || !strings.Contains(got.Detail, "records a deletion") {
		t.Fatalf("verdict = %+v, want content_mismatch naming the deletion", got)
	}
}

// TestGetSessionSurfacesIntegrityAndStampsBreak: the session load path runs
// the verification, surfaces the named verdict, and durably stamps the tamper
// flag on a broken chain (the MarkChainBroken mirror) — never silently.
func TestGetSessionSurfacesIntegrityAndStampsBreak(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	msgs := []agent.Message{agent.TextMessage(agent.RoleUser, "hello")}
	data, _ := json.Marshal(msgs)
	spill := &fakeSpill{objs: map[string][]byte{"s1:transcript": data}}
	refJSON, _ := json.Marshal(audit.SpillRef{Backend: "fake", Key: "s1:transcript"})
	h := New(Config{Pool: mock, Spill: spill})

	// Load (transcript), then verify: the anchor claims 2 revisions but the
	// chain is empty → chain_broken → the flag write follows.
	mock.ExpectQuery(`SELECT "spillRef", "createdAt", "updatedAt" FROM "AssistantSession"`).
		WithArgs("s1", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"spillRef", "createdAt", "updatedAt"}).AddRow(refJSON, rowT, rowT))
	expectSessionAnchor(mock, refJSON, 2, "some-head")
	expectChatEvents(mock)
	mock.ExpectExec(`UPDATE "AssistantSession" SET "chainBrokenAt"`).
		WithArgs("s1", "alice").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	e := echo.New()
	c, rec := ctxWithUser(e, http.MethodGet, "/s", "alice")
	c.SetParamNames("id")
	c.SetParamValues("s1")
	if err := h.GetSession(c); err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d (a broken chain is served WITH the break named, not refused)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"chain_broken"`) {
		t.Fatalf("response must name the chain break: %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the chain break was not durably stamped: %v", err)
	}
}

// TestGetSessionVerifiedIntegrity: an intact chain reloads as verified.
func TestGetSessionVerifiedIntegrity(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	msgs := []agent.Message{agent.TextMessage(agent.RoleUser, "hello")}
	data, _ := json.Marshal(msgs)
	spill := &fakeSpill{objs: map[string][]byte{"s1:transcript": data}}
	refJSON, _ := json.Marshal(audit.SpillRef{Backend: "fake", Key: "s1:transcript"})
	h := New(Config{Pool: mock, Spill: spill})

	e1 := mkChatEvent(t, nil, 1, chatKindRevision, chatOriginWeb, 1, audit.SHA256Hex(data))
	mock.ExpectQuery(`SELECT "spillRef", "createdAt", "updatedAt" FROM "AssistantSession"`).
		WithArgs("s1", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"spillRef", "createdAt", "updatedAt"}).AddRow(refJSON, rowT, rowT))
	expectSessionAnchor(mock, refJSON, 1, e1.Hash)
	expectChatEvents(mock, e1)

	e := echo.New()
	c, rec := ctxWithUser(e, http.MethodGet, "/s", "alice")
	c.SetParamNames("id")
	c.SetParamValues("s1")
	if err := h.GetSession(c); err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"status":"verified"`) {
		t.Fatalf("response must carry the verified verdict: %s", rec.Body.String())
	}
}

// TestAppendChatEvent_ExhaustedRetries: persistent seq contention fails with a
// named error instead of forking or spinning.
func TestAppendChatEvent_ExhaustedRetries(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	for range chatChainAppendAttempts {
		mock.ExpectQuery(`SELECT "seq","hash" FROM "AssistantChatEvent"`).
			WithArgs("s1", "alice").WillReturnError(pgx.ErrNoRows)
		mock.ExpectExec(`INSERT INTO "AssistantChatEvent"`).
			WithArgs("s1", "alice", 1, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
				pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnError(&pgconn.PgError{Code: "23505"})
	}
	_, _, err := appendChatEvent(context.Background(), mock, "alice", "s1", chatKindRevision, 1, "d")
	if err == nil || !strings.Contains(err.Error(), "exhausted retries") {
		t.Fatalf("err = %v, want a named retry exhaustion", err)
	}
}

// TestVerifySessionChain_ReadFailuresAreNamed: infrastructure failures surface
// as named verdicts, never as silently verified.
func TestVerifySessionChain_ReadFailuresAreNamed(t *testing.T) {
	t.Run("anchor unreadable", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		s := newDBStore(context.Background(), mock, &fakeSpill{}, "alice")
		mock.ExpectQuery(`SELECT "spillRef","chainSeq","chainHead" FROM "AssistantSession"`).
			WithArgs("s1", "alice").WillReturnError(pgx.ErrNoRows)
		if got := s.verifySessionChain("s1"); got.Status != chatIntegrityChainBroken || !strings.Contains(got.Detail, "anchor could not be read") {
			t.Fatalf("verdict = %+v", got)
		}
	})
	t.Run("chain unreadable", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		s, _, refJSON := chatVerifyFixture(t, mock)
		expectSessionAnchor(mock, refJSON, 1, "h")
		mock.ExpectQuery(`SELECT "seq","kind","contentDigest","prevHash","hash","hashInput"`).
			WithArgs("s1", "alice").WillReturnError(pgx.ErrNoRows)
		if got := s.verifySessionChain("s1"); got.Status != chatIntegrityChainBroken || !strings.Contains(got.Detail, "audit chain could not be read") {
			t.Fatalf("verdict = %+v", got)
		}
	})
}

// TestVerifySessionChain_ColumnEnvelopeDriftDetected: a convenience column
// edited away from its hash-covered envelope is itself evidence.
func TestVerifySessionChain_ColumnEnvelopeDriftDetected(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	s, chain, refJSON := chatVerifyFixture(t, mock)
	chain[1].ContentDigest = audit.SHA256Hex([]byte("laundered"))
	expectSessionAnchor(mock, refJSON, 2, chain[1].Hash)
	expectChatEvents(mock, chain...)
	got := s.verifySessionChain("s1")
	if got.Status != chatIntegrityChainBroken || !strings.Contains(got.Detail, "disagree with its hash-covered envelope") {
		t.Fatalf("verdict = %+v, want chain_broken on column/envelope drift", got)
	}
}

// TestVerifySessionChain_UndecodableHeadEnvelope: a head whose stored envelope
// is not decodable JSON cannot attest anything — named chain break.
func TestVerifySessionChain_UndecodableHeadEnvelope(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	s, _, refJSON := chatVerifyFixture(t, mock)
	raw := []byte(`"not an object"`)
	bad := chatChainRow{Seq: 1, Kind: chatKindRevision, HashInput: raw, Hash: hashchain.ChainHash(nil, raw)}
	expectSessionAnchor(mock, refJSON, 1, bad.Hash)
	expectChatEvents(mock, bad)
	got := s.verifySessionChain("s1")
	if got.Status != chatIntegrityChainBroken || !strings.Contains(got.Detail, "undecodable") {
		t.Fatalf("verdict = %+v, want chain_broken (undecodable envelope)", got)
	}
}

// TestVerifySessionChain_TranscriptStates: the content cross-check tolerates a
// missing blob (swept) but names an unreadable one.
func TestVerifySessionChain_TranscriptStates(t *testing.T) {
	t.Run("unreadable spill ref", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		s, chain, _ := chatVerifyFixture(t, mock)
		expectSessionAnchor(mock, []byte("{not json"), 2, chain[1].Hash)
		expectChatEvents(mock, chain...)
		if got := s.verifySessionChain("s1"); got.Status != chatIntegrityContentMismatch || !strings.Contains(got.Detail, "could not be read") {
			t.Fatalf("verdict = %+v", got)
		}
	})
	t.Run("spill backend error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		s, chain, refJSON := chatVerifyFixture(t, mock)
		s.spill.(*fakeSpill).getErr = context.DeadlineExceeded
		expectSessionAnchor(mock, refJSON, 2, chain[1].Hash)
		expectChatEvents(mock, chain...)
		if got := s.verifySessionChain("s1"); got.Status != chatIntegrityContentMismatch || !strings.Contains(got.Detail, "could not be read") {
			t.Fatalf("verdict = %+v", got)
		}
	})
	t.Run("null ref counts as swept", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		s, chain, _ := chatVerifyFixture(t, mock)
		expectSessionAnchor(mock, []byte("null"), 2, chain[1].Hash)
		expectChatEvents(mock, chain...)
		if got := s.verifySessionChain("s1"); got.Status != chatIntegrityVerified || !strings.Contains(got.Detail, "expired under retention") {
			t.Fatalf("verdict = %+v", got)
		}
	})
}

// TestDeleteSessionTombstoneFailureIs500: a deleted row whose audit tombstone
// failed is an infrastructure fault (500), never disguised as not-found.
func TestDeleteSessionTombstoneFailureIs500(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	h := New(Config{Pool: mock, Spill: &fakeSpill{}})
	mock.ExpectQuery(`DELETE FROM "AssistantSession" WHERE id = \$1 AND "userId" = \$2 RETURNING "spillRef"`).
		WithArgs("s1", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"spillRef"}).AddRow([]byte("null")))
	mock.ExpectQuery(`SELECT "seq","hash" FROM "AssistantChatEvent"`).
		WithArgs("s1", "alice").WillReturnError(context.DeadlineExceeded)
	e := echo.New()
	c, rec := ctxWithUser(e, http.MethodDelete, "/s", "alice")
	c.SetParamNames("id")
	c.SetParamValues("s1")
	_ = h.DeleteSession(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500 (audit gap is not a 404)", rec.Code)
	}
}

// TestVerifySessionChain_LaggingAnchorStillPinsItsEntry (review F1): a full
// self-consistent chain rewrite cannot hide behind one appended junk entry —
// when the anchor lags the head, the anchored entry's hash must still equal
// the anchor, or the chain is broken.
func TestVerifySessionChain_LaggingAnchorStillPinsItsEntry(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	s, chain, refJSON := chatVerifyFixture(t, mock)
	// Attacker rewrites the whole chain self-consistently (different digests),
	// then appends one junk entry so the anchor (rev 2) lags the head (rev 3).
	r1 := mkChatEvent(t, nil, 1, chatKindRevision, chatOriginWeb, 1, "forged-1")
	r2 := mkChatEvent(t, &r1.Hash, 2, chatKindRevision, chatOriginWeb, 1, "forged-2")
	r3 := mkChatEvent(t, &r2.Hash, 3, chatKindRevision, chatOriginWeb, 1, "forged-3")
	expectSessionAnchor(mock, refJSON, 2, chain[1].Hash) // the REAL anchor
	expectChatEvents(mock, r1, r2, r3)
	got := s.verifySessionChain("s1")
	if got.Status != chatIntegrityChainBroken || !strings.Contains(got.Detail, "anchor disagrees") {
		t.Fatalf("verdict = %+v, want chain_broken (rewritten chain behind a lagging anchor)", got)
	}
}

// TestVerifySessionChain_LegitimateAnchorLagVerifies (review F3): an append
// whose row-stamp did not land (anchor lags by one) is an infra blip, not
// tampering — the served transcript matches its ANCHORED entry and the verdict
// is verified, naming the unacknowledged revisions.
func TestVerifySessionChain_LegitimateAnchorLagVerifies(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	s, chain, refJSON := chatVerifyFixture(t, mock)
	// The chain gained rev 3 (digest of newer content) but the session row
	// still serves rev 2's content and anchors rev 2.
	e3 := mkChatEvent(t, &chain[1].Hash, 3, chatKindRevision, chatOriginWeb, 2, "newer-digest-never-stamped")
	expectSessionAnchor(mock, refJSON, 2, chain[1].Hash)
	expectChatEvents(mock, chain[0], chain[1], e3)
	got := s.verifySessionChain("s1")
	if got.Status != chatIntegrityVerified || !strings.Contains(got.Detail, "await the session row's acknowledgment") {
		t.Fatalf("verdict = %+v, want verified naming the anchor lag", got)
	}
}
