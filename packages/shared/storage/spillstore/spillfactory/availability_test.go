package spillfactory

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
)

// The silent failure this whole file guards: an admin sets an inline-vs-spill
// threshold, the control reads as configured, and nothing spills because no
// backend exists. Describe is the only place that says so.

func TestDescribe_NoBackend_SaysTheThresholdIsInert(t *testing.T) {
	got := Describe(FactoryConfig{}, nil)

	if got.Configured {
		t.Fatal("Configured is true with no store built; an admin would read this as a working spill backend")
	}
	if got.Backend != "" || got.Location != "" {
		t.Fatalf("a non-existent backend reported a name/location: %+v", got)
	}
	if !strings.Contains(got.Effect, "kept inline") {
		t.Fatalf("Effect does not state the consequence — that the threshold does nothing: %q", got.Effect)
	}
	if !strings.Contains(got.Effect, "spill.enabled") {
		t.Fatalf("Effect names no remedy, so an operator learns the state but not the fix: %q", got.Effect)
	}
}

// localfs must be reported as host-local. Claiming otherwise is the exact
// misconfiguration that makes every spilled body unreadable from another node
// while every write succeeds.
func TestDescribe_Localfs_IsMarkedHostLocal(t *testing.T) {
	cfg := FactoryConfig{Enabled: true, Backend: "localfs"}
	cfg.Localfs.Root = "/var/lib/nexus/spill"
	got := Describe(cfg, stubStore{backend: "localfs"})

	if !got.Configured {
		t.Fatal("a built localfs store reported Configured=false")
	}
	if !got.HostLocal {
		t.Fatal("localfs reported as NOT host-local; a multi-node deployment would look correct while every cross-node read fails")
	}
	if got.Location != "/var/lib/nexus/spill" {
		t.Fatalf("Location = %q, want the configured root — comparing roots across nodes is how the misconfiguration is spotted", got.Location)
	}
	if !strings.Contains(got.Effect, "same root") {
		t.Fatalf("Effect does not state the shared-root requirement: %q", got.Effect)
	}
}

// The mirror: s3 is shared, and must not inherit the host-local warning or an
// operator would go mount volumes for a problem they do not have.
func TestDescribe_S3_IsNotHostLocal(t *testing.T) {
	cfg := FactoryConfig{Enabled: true, Backend: "s3"}
	cfg.S3.Bucket = "nexus-audit"
	cfg.S3.Prefix = "spill/"
	got := Describe(cfg, stubStore{backend: "s3"})

	if got.HostLocal {
		t.Fatal("s3 reported as host-local; every node can read a bucket")
	}
	if got.Location != "nexus-audit/spill/" {
		t.Fatalf("Location = %q, want bucket/prefix", got.Location)
	}
}

func TestDescribe_S3_WithoutPrefix_ReportsBucketAlone(t *testing.T) {
	cfg := FactoryConfig{Enabled: true, Backend: "s3"}
	cfg.S3.Bucket = "nexus-audit"
	got := Describe(cfg, stubStore{backend: "s3"})

	if got.Location != "nexus-audit" {
		t.Fatalf("Location = %q, want the bare bucket when no prefix is set", got.Location)
	}
}

// Async acknowledges a write before the object lands, so a crash leaves a
// reference to an object that was never written. An operator diagnosing a
// not-found spilled body needs that stated, not inferred.
func TestDescribe_Async_StatesTheDurabilityCaveat(t *testing.T) {
	cfg := FactoryConfig{Enabled: true, Backend: "s3", Async: true}
	cfg.S3.Bucket = "b"
	got := Describe(cfg, stubStore{backend: "s3"})

	if !got.Async {
		t.Fatal("Async config not reflected in the snapshot")
	}
	if !strings.Contains(got.Effect, "never written") {
		t.Fatalf("Effect omits the async durability caveat: %q", got.Effect)
	}
}

// An unrecognised backend must not be guessed at. Reporting an unknown backend
// as shared when it is host-local is precisely the wrong answer to give.
func TestDescribe_UnknownBackend_MakesNoLocalityClaim(t *testing.T) {
	got := Describe(FactoryConfig{Enabled: true}, stubStore{backend: "gcs"})

	if got.HostLocal {
		t.Fatal("an unknown backend was assumed host-local")
	}
	if got.Location != "" {
		t.Fatalf("Location = %q, want empty — the config holds no options for an unknown backend", got.Location)
	}
	if !strings.Contains(got.Effect, "gcs") {
		t.Fatalf("Effect does not name the backend actually in force: %q", got.Effect)
	}
}

// Describe reads the store's OWN Backend() rather than the configured string,
// so a mismatch between what was asked for and what was built surfaces as the
// truth rather than as the intent.
func TestDescribe_ReportsTheBuiltBackendNotTheConfiguredOne(t *testing.T) {
	got := Describe(FactoryConfig{Enabled: true, Backend: "s3"}, stubStore{backend: "localfs"})

	if got.Backend != "localfs" {
		t.Fatalf("Backend = %q, want the store's own name — reporting the configured value would hide a wiring bug", got.Backend)
	}
	if !got.HostLocal {
		t.Fatal("locality was derived from the configured backend rather than the built one")
	}
}

// stubStore reports a backend name and nothing else — Describe reads only
// Backend(), so the rest of the interface is inert here by design.
type stubStore struct{ backend string }

func (s stubStore) Backend() string { return s.backend }
func (s stubStore) Put(_ context.Context, _ io.Reader, _ int64, _ spillstore.PutOptions) (audit.SpillRef, error) {
	return audit.SpillRef{}, nil
}
func (s stubStore) Get(_ context.Context, _ audit.SpillRef) (io.ReadCloser, error) { return nil, nil }
func (s stubStore) Delete(_ context.Context, _ audit.SpillRef) error               { return nil }
func (s stubStore) Sweep(_ context.Context, _ time.Time) (int, error)              { return 0, nil }
func (s stubStore) Stat(_ context.Context) (spillstore.Stats, error) {
	return spillstore.Stats{}, nil
}
