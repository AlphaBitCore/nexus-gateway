package runtime

import (
	"context"
	"fmt"
	"strings"
)

// The mitigate tools accept a human-friendly identifier (a provider name, a rule
// name, or a Virtual Key name / key-prefix / id) and resolve it to the id the
// admin write endpoint needs — so an agent never has to know or pass a bare id.
// Resolution is case-insensitive. An unknown name fails with the list of valid
// names, and an AMBIGUOUS name (more than one entity matches) is refused rather
// than silently mutating the first match — the wrong VK revoked or provider
// disabled is not recoverable by undo.

// resolveCandidate is one entity weighed during resolution.
type resolveCandidate struct {
	id       string
	label    string       // display label to echo back
	known    string       // name listed in a not-found error ("" => omitted)
	matched  bool         // whether the caller's ref matched this entity
	validate func() error // optional post-match check (e.g. a VK must be active)
}

// resolveUnique picks the single matching candidate, or returns a not-found /
// ambiguous error built from kind ("provider", "routing rule", "virtual key") and
// the caller's ref — the one shape behind all three entity resolvers. An ambiguous
// reference is refused, never resolved to the first match.
func resolveUnique(kind, ref string, cands []resolveCandidate) (string, string, error) {
	var known []string
	var got resolveCandidate
	matches := 0
	for _, c := range cands {
		if c.known != "" {
			known = append(known, c.known)
		}
		if c.matched {
			got = c
			matches++
		}
	}
	switch matches {
	case 1:
		if got.validate != nil {
			if err := got.validate(); err != nil {
				return "", "", err
			}
		}
		return got.id, got.label, nil
	case 0:
		return "", "", fmt.Errorf("no %s matching %q; known: %s", kind, ref, strings.Join(known, ", "))
	default:
		return "", "", fmt.Errorf("%s %q is ambiguous (%d matches) — resolve it from the interactive console instead", kind, ref, matches)
	}
}

// resolveProviderID maps a provider name or display name to its id + the display
// label to echo back. Ambiguous matches are refused.
func resolveProviderID(ctx context.Context, gw Gateway, name string) (string, string, error) {
	res, err := gw.Providers(ctx)
	if err != nil {
		return "", "", err
	}
	name = strings.TrimSpace(name)
	cands := make([]resolveCandidate, 0, len(res.Data))
	for _, p := range res.Data {
		lbl := p.DisplayName
		if lbl == "" {
			lbl = p.Name
		}
		cands = append(cands, resolveCandidate{
			id: p.ID, label: lbl, known: lbl,
			matched: strings.EqualFold(p.Name, name) || strings.EqualFold(p.DisplayName, name),
		})
	}
	return resolveUnique("provider", name, cands)
}

// resolveRuleID maps a routing-rule name to its id. Ambiguous names are refused
// (routing-rule names are not unique-constrained).
func resolveRuleID(ctx context.Context, gw Gateway, name string) (string, string, error) {
	rules, err := gw.RoutingRules(ctx)
	if err != nil {
		return "", "", err
	}
	name = strings.TrimSpace(name)
	cands := make([]resolveCandidate, 0, len(rules))
	for _, r := range rules {
		cands = append(cands, resolveCandidate{
			id: r.ID, label: r.Name, known: r.Name,
			matched: strings.EqualFold(r.Name, name),
		})
	}
	return resolveUnique("routing rule", name, cands)
}

// resolveRevocableVK maps a Virtual Key name, key-prefix, or id to its id. Only a
// key in active status is returned — the revoke endpoint 404s on any other, so
// resolving to a non-revocable key is reported as a clear error. An ambiguous
// reference (e.g. one key's name equals another's prefix) is refused.
func resolveRevocableVK(ctx context.Context, gw Gateway, ref string) (string, string, error) {
	vks, err := gw.VirtualKeys(ctx)
	if err != nil {
		return "", "", err
	}
	ref = strings.TrimSpace(ref)
	cands := make([]resolveCandidate, 0, len(vks))
	for _, v := range vks {
		v := v
		label := v.Name
		if label == "" {
			label = v.KeyPrefix
		}
		cands = append(cands, resolveCandidate{
			id: v.ID, label: label, known: v.Name,
			matched: strings.EqualFold(v.Name, ref) || strings.EqualFold(v.KeyPrefix, ref) || v.ID == ref,
			validate: func() error {
				if !v.Revocable() {
					return fmt.Errorf("virtual key %q is %s, not active — only active keys can be revoked", ref, v.Status())
				}
				return nil
			},
		})
	}
	return resolveUnique("virtual key", ref, cands)
}
