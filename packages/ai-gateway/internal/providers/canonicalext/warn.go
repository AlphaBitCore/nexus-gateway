package canonicalext

import (
	"context"
	"log/slog"
	"sync"

	"github.com/tidwall/gjson"
)

// seenWarn dedupes (provider, field) WARN entries process-wide. The set is
// monotonically growing — a process emits at most one WARN per pair, so
// operators see drift signals without log flooding.
var seenWarn sync.Map

// WarnOnce emits a slog.LevelWarn entry for the given (provider, field)
// pair the first time the pair is observed in this process; subsequent
// calls return without logging. Use it from codec EncodeRequest paths
// (and other translation surfaces) to flag canonical fields that the
// target wire format cannot represent. Callers add structured context
// via attrs (e.g. message index, raw value) — those still appear on the
// emitted record.
func WarnOnce(provider, field string, attrs ...slog.Attr) {
	key := provider + "|" + field
	if _, loaded := seenWarn.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	all := append([]slog.Attr{
		slog.String("event", "nexus_field_unsupported"),
		slog.String("provider", provider),
		slog.String("field", field),
	}, attrs...)
	slog.Default().LogAttrs(context.Background(), slog.LevelWarn,
		"nexus: dropping unsupported canonical field", all...)
}

// ScanUnsupported walks the top-level keys of canonicalBody and emits a
// [WarnOnce] for every field not present in supported. The nexus.ext
// passthrough namespace is always allowed and never warned. Intended to
// run at the end of [providers.SchemaCodec.EncodeRequest] so an operator
// can spot quietly dropped canonical fields without per-codec audit
// logging plumbing.
func ScanUnsupported(provider string, canonicalBody []byte, supported map[string]struct{}) {
	if len(canonicalBody) == 0 {
		return
	}
	gjson.ParseBytes(canonicalBody).ForEach(func(k, _ gjson.Result) bool {
		field := k.String()
		if field == "" || field == "nexus" {
			return true
		}
		// Treat keys starting with "_" or "$" as private metadata
		// (e.g. fixture provenance comments, vendor extensions) so
		// production drift signals stay clean.
		if first := field[0]; first == '_' || first == '$' {
			return true
		}
		if _, ok := supported[field]; ok {
			return true
		}
		WarnOnce(provider, field)
		return true
	})
}

// ResetWarnSeenForTest clears the dedup set so unit tests can exercise
// the WARN path repeatedly. NOT FOR PRODUCTION CALLERS — flushing the set
// in a running gateway would re-emit every previously warned field.
func ResetWarnSeenForTest() {
	seenWarn = sync.Map{}
}
