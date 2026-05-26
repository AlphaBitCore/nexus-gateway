package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// AuditFn is the signature ai-gateway's audit.Writer expects from its
// normalize hook. It is decoupled from audit.NormalizeFn to keep that
// package free of a shared/normalize dependency — the binary wires the
// two via this bridge.
//
// adapterType is the wire-format key from routing ("openai", "anthropic",
// "gemini", ...). The audit layer must resolve provider name → adapter
// type before invoking; see audit.normalizeAdapterType.
type AuditFn func(direction, contentType, adapterType, model, path string, stream bool, body []byte) (json.RawMessage, string, string)

// BuildAuditFn constructs an AuditFn that:
//   - resolves the right Normalizer via the registry,
//   - invokes Normalize,
//   - marshals the result to JSON,
//   - classifies the outcome as "ok" / "partial" / "failed",
//   - records prometheus metrics under `metrics` (may be nil).
//
// Returns (nil, "", "") for absent / empty bodies so callers can leave
// the wire fields unset.
func BuildAuditFn(reg *Registry, metrics *Metrics) AuditFn {
	if reg == nil {
		return nil
	}
	return func(direction, contentType, adapterType, model, path string, stream bool, body []byte) (json.RawMessage, string, string) {
		if len(body) == 0 {
			return nil, "", ""
		}
		meta := Meta{
			AdapterType:  strings.ToLower(adapterType),
			Model:        model,
			ContentType:  stripContentTypeParams(contentType),
			Direction:    Direction(direction),
			EndpointPath: path,
			Stream:       stream,
		}
		payload, err := reg.Normalize(context.Background(), body, meta)

		adapter := payload.Protocol
		if adapter == "" {
			adapter = "unsupported"
		}
		kind := string(payload.Kind)
		if kind == "" {
			kind = string(KindUnsupported)
		}

		var (
			status    string
			errReason string
			raw       json.RawMessage
		)
		switch {
		case err == nil:
			status = "ok"
		case errors.Is(err, ErrUnsupported):
			status = "failed"
			errReason = err.Error()
			if metrics != nil {
				metrics.FallbackTotal.WithLabelValues("unsupported").Inc()
			}
		default:
			// Partial: normalizer produced a payload but parsing was
			// incomplete (e.g. truncated stream). Persist what we have.
			status = "partial"
			errReason = err.Error()
		}

		if status != "failed" {
			b, mErr := json.Marshal(payload)
			if mErr != nil {
				status = "failed"
				errReason = "marshal normalized payload: " + mErr.Error()
				raw = nil
				if metrics != nil {
					metrics.FallbackTotal.WithLabelValues("marshal-error").Inc()
				}
			} else {
				raw = b
			}
		}

		if metrics != nil {
			metrics.Total.WithLabelValues(adapter, kind, direction, status).Inc()
			if status != "failed" && len(raw) > 0 {
				metrics.PayloadBytes.WithLabelValues(adapter, direction).Observe(float64(len(raw)))
			}
		}
		return raw, status, errReason
	}
}

// StripContentTypeParams removes "; charset=utf-8" etc. so registry
// keys ("application/json:/v1/chat/completions") stay stable.
// Exported so the codecs and tests sub-packages can reuse the same logic.
func StripContentTypeParams(ct string) string {
	return stripContentTypeParams(ct)
}

// stripContentTypeParams removes "; charset=utf-8" etc. so registry
// keys ("application/json:/v1/chat/completions") stay stable.
func stripContentTypeParams(ct string) string {
	if ct == "" {
		return ""
	}
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		return strings.TrimSpace(ct[:i])
	}
	return strings.TrimSpace(ct)
}

// MustRegisterPrometheus is a small convenience wrapper used by binaries
// that want the registered metric set with a sensible namespace and
// promauto registration order. Returns nil when reg is nil (test path).
func MustRegisterPrometheus(reg prometheus.Registerer, namespace string) *Metrics {
	if reg == nil {
		return nil
	}
	return NewMetrics(reg, namespace)
}
