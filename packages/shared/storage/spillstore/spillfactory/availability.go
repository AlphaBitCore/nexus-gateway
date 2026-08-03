package spillfactory

import (
	"context"
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
)

// Availability answers, for one running process, the question no admin surface
// could answer before: **is there actually somewhere for an oversize body to
// go?**
//
// The inline-vs-spill threshold is an admin setting and the raw-body spill
// switch is an admin toggle, but the backend that both depend on is per-service
// boot config. Set the threshold with no backend wired and every control reads
// as configured while every body is kept inline — the setting is not wrong, it
// is inert, and nothing said so.
//
// Location is included deliberately. Two nodes each running their own localfs
// root is a working configuration for writes and a broken one for reads, and it
// is only visible by comparing the roots across nodes. Surfacing it here lets an
// operator see that before a body goes missing, rather than afterwards from the
// read-failure cause (`not_found_host_local`).
//
// It carries no secret: S3 credentials come from the AWS SDK default chain and
// are deliberately never in this config.
type Availability struct {
	// Configured reports whether this process actually holds a spill backend.
	// This is the load-bearing field; everything else is detail.
	Configured bool `json:"configured"`

	// Backend is the backend name in force ("localfs", "s3"), empty when none.
	Backend string `json:"backend,omitempty"`

	// Location is where spilled bytes land — the localfs root, or the S3
	// bucket and prefix. Empty when no backend is configured.
	Location string `json:"location,omitempty"`

	// HostLocal reports whether the backend is readable only on this host. A
	// localfs root is; an S3 bucket is not. When true, a multi-node deployment
	// needs a shared volume at the same path or a shared backend, or spilled
	// bodies will be unreadable from every other node.
	HostLocal bool `json:"hostLocal"`

	// Async reports whether writes are acknowledged before the object lands.
	Async bool `json:"async"`

	// Effect states the consequence in plain language, so the snapshot is
	// readable without knowing the subsystem.
	Effect string `json:"effect"`
	// Residency is what is actually sitting in the backend right now — object
	// count, bytes, and the age window — or nil when it could not be measured.
	// Nil is deliberate rather than a zero struct: "we could not look" and "the
	// store is empty" are different answers, and a zero-valued count reads as the
	// second when it means the first. Populated by DescribeWithResidency.
	Residency *Residency `json:"residency,omitempty"`
}

// Describe reports the spill posture of a running process from its boot config
// and the store the factory actually built.
//
// store is the value New returned: nil means no backend, which is the case the
// whole type exists to make visible.
//
// It does not call SpillStore.Stat: this function answers "where would a body
// go?", which is a boot-time fact and cannot fail. The measured contents are a
// separate question with a separate failure mode, so they live in
// DescribeWithResidency — which does call Stat, now that both backends bound the
// scan and honour their context.
func Describe(cfg FactoryConfig, store spillstore.SpillStore) Availability {
	if store == nil {
		return Availability{
			Configured: false,
			Effect: "No spill backend is configured on this node, so every captured body is kept " +
				"inline regardless of the inline-vs-spill threshold. Set spill.enabled in this " +
				"service's config to make the threshold take effect.",
		}
	}

	a := Availability{
		Configured: true,
		Backend:    store.Backend(),
		Async:      cfg.Async,
	}
	switch a.Backend {
	case "localfs":
		a.Location = cfg.Localfs.Root
		a.HostLocal = true
		a.Effect = fmt.Sprintf(
			"Bodies at or above the inline-vs-spill threshold are written to %q on this host. "+
				"A localfs object is readable only by a process that can see this path, so every "+
				"node in a multi-node deployment must mount the same root — otherwise spilled "+
				"bodies are unreadable from the Control Plane.", cfg.Localfs.Root)
	case "s3":
		a.Location = cfg.S3.Bucket
		if cfg.S3.Prefix != "" {
			a.Location = cfg.S3.Bucket + "/" + cfg.S3.Prefix
		}
		a.Effect = fmt.Sprintf(
			"Bodies at or above the inline-vs-spill threshold are written to the %q bucket, "+
				"which every node can read.", cfg.S3.Bucket)
	default:
		// A backend the factory does not build today. Report it honestly rather
		// than guessing its locality — claiming a shared backend that is really
		// host-local is the failure this type exists to prevent.
		a.Effect = fmt.Sprintf("Bodies at or above the inline-vs-spill threshold are written to the %q backend.", a.Backend)
	}
	if cfg.Async {
		a.Effect += " Writes are acknowledged before the object lands, so a crash can leave a " +
			"reference to an object that was never written."
	}
	return a
}

// Residency is the measured contents of the spill backend, as reported by
// SpillStore.Stat. It exists because the rest of Availability answers "where
// would a body go?" and an operator asking that usually also needs "and how much
// is already there?" — retention, disk pressure and a stale-root misconfiguration
// all show up here first.
type Residency struct {
	ObjectCount int64  `json:"objectCount"`
	TotalBytes  int64  `json:"totalBytes"`
	OldestAt    string `json:"oldestAt,omitempty"`
	NewestAt    string `json:"newestAt,omitempty"`
	// Truncated reports that the counts are LOWER BOUNDS: the backend stopped at
	// its scan limit. Reported rather than hidden, because a partial count that
	// looks total is worse than no count.
	Truncated bool `json:"truncated,omitempty"`
	ScanLimit int  `json:"scanLimit,omitempty"`
	// Error carries why the measurement failed when it did (a timeout, an
	// unreadable root). Set with Residency non-nil ONLY when partial numbers were
	// still gathered; a total failure leaves Residency nil.
	Error string `json:"error,omitempty"`
}

// residencyTimeout bounds the measurement. Stat is reachable from an
// operator-facing introspection call, and the surface an operator reaches for
// when something is wrong must not be the surface that hangs.
const residencyTimeout = 2 * time.Second

// DescribeWithResidency is Describe plus a bounded Stat of what the backend
// currently holds. It is the first production consumer of SpillStore.Stat, which
// previously had none on a shipped interface — the reason it had none is that
// neither backend bounded its scan, so the obvious consumer could not safely be
// the first one. Both bound now, and this call adds its own deadline on top.
//
// A Stat failure never fails the description: the caller asked where bodies go,
// and that answer does not depend on being able to count them.
func DescribeWithResidency(ctx context.Context, cfg FactoryConfig, store spillstore.SpillStore) Availability {
	a := Describe(cfg, store)
	if store == nil {
		return a
	}
	statCtx, cancel := context.WithTimeout(ctx, residencyTimeout)
	defer cancel()
	st, err := store.Stat(statCtx)
	if err != nil && st.ObjectCount == 0 && st.TotalBytes == 0 {
		// Nothing measured at all — leave Residency nil so a reader cannot mistake
		// a failed look for an empty store, and carry the reason on Effect.
		a.Effect += fmt.Sprintf(" Residency could not be measured: %v.", err)
		return a
	}
	r := &Residency{
		ObjectCount: st.ObjectCount,
		TotalBytes:  st.TotalBytes,
		Truncated:   st.Truncated,
		ScanLimit:   st.ScanLimit,
	}
	if !st.OldestAt.IsZero() {
		r.OldestAt = st.OldestAt.UTC().Format(time.RFC3339)
	}
	if !st.NewestAt.IsZero() {
		r.NewestAt = st.NewestAt.UTC().Format(time.RFC3339)
	}
	if err != nil {
		r.Error = err.Error()
	}
	a.Residency = r
	return a
}
