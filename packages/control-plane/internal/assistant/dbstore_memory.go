package assistant

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

// dbMemory is the web MemoryStore: durable per-user facts in Postgres, isolated by
// userId. Constructed per caller bound to the authenticated userId; every query
// carries WHERE "userId".
type dbMemory struct {
	pool   pgxPool
	userID string
	ctx    context.Context
}

func newDBMemory(ctx context.Context, pool pgxPool, userID string) *dbMemory {
	return &dbMemory{pool: pool, userID: userID, ctx: ctx}
}

func (m *dbMemory) Index() (string, error) {
	rows, err := m.pool.Query(m.ctx,
		`SELECT name, type, body FROM "AssistantMemory" WHERE "userId" = $1 ORDER BY name`, m.userID)
	if err != nil {
		return "", fmt.Errorf("memory index: %w", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var name, typ, body string
		if err := rows.Scan(&name, &typ, &body); err != nil {
			return "", fmt.Errorf("scan memory: %w", err)
		}
		fmt.Fprintf(&b, "- %s [%s] — %s\n", name, typ, oneLineDB(body))
	}
	return strings.TrimRight(b.String(), "\n"), rows.Err()
}

func (m *dbMemory) Recall(name string) (agent.MemoryFact, bool, error) {
	var f agent.MemoryFact
	err := m.pool.QueryRow(m.ctx,
		`SELECT name, type, body FROM "AssistantMemory" WHERE "userId" = $1 AND name = $2`,
		m.userID, agent.Slugify(name)).Scan(&f.Name, &f.Type, &f.Body)
	if errors.Is(err, pgx.ErrNoRows) {
		return agent.MemoryFact{}, false, nil
	}
	if err != nil {
		return agent.MemoryFact{}, false, fmt.Errorf("recall: %w", err)
	}
	return f, true, nil
}

func (m *dbMemory) Remember(f agent.MemoryFact) error {
	// Key by the slug so a fact has ONE identity across cloud and device (C1): the
	// device names a fact's file by its slug, so the cloud must store + sync the same.
	_, err := m.pool.Exec(m.ctx, `
		INSERT INTO "AssistantMemory" ("userId", name, type, body, "updatedAt")
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT ("userId", name) DO UPDATE SET type = EXCLUDED.type, body = EXCLUDED.body, "updatedAt" = now()`,
		m.userID, agent.Slugify(f.Name), f.Type, f.Body)
	if err != nil {
		return fmt.Errorf("remember: %w", err)
	}
	return nil
}

func (m *dbMemory) Update(name, body string) error {
	tag, err := m.pool.Exec(m.ctx,
		`UPDATE "AssistantMemory" SET body = $3, "updatedAt" = now() WHERE "userId" = $1 AND name = $2`,
		m.userID, agent.Slugify(name), body)
	if err != nil {
		return fmt.Errorf("update memory: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no memory named %q to update", name)
	}
	return nil
}

func (m *dbMemory) Forget(name string) (bool, error) {
	tag, err := m.pool.Exec(m.ctx,
		`DELETE FROM "AssistantMemory" WHERE "userId" = $1 AND name = $2`, m.userID, agent.Slugify(name))
	if err != nil {
		return false, fmt.Errorf("forget: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

var _ agent.MemoryStore = (*dbMemory)(nil)
