package audit

import (
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// TestAuditFreshnessCheck_Identity verifies ID, Name, Description, Interval,
// and RunOnStart accessors through the direct-construction path.
func TestAuditFreshnessCheck_IdentityDirect(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	j := &AuditFreshnessCheck{
		pool:      mock,
		interval:  2 * time.Minute,
		threshold: 10 * time.Minute,
		logger:    testLogger().With("job", auditFreshnessJobID),
	}

	if j.ID() != auditFreshnessJobID {
		t.Errorf("ID = %q; want %q", j.ID(), auditFreshnessJobID)
	}
	if j.Name() == "" {
		t.Error("Name must be non-empty")
	}
	if j.Description() == "" {
		t.Error("Description must be non-empty")
	}
	if j.Interval() != 2*time.Minute {
		t.Errorf("Interval = %v; want 2m", j.Interval())
	}
	if j.RunOnStart() {
		t.Error("RunOnStart must be false")
	}
}
