package mcp

import (
	"context"
	"fmt"
	"strings"
)

// The mitigate tools accept a human-friendly identifier (a provider name, a rule
// name, or a Virtual Key name / key-prefix / id) and resolve it to the id the
// admin write endpoint needs — so an agent never has to know or pass a bare id.
// Resolution is case-insensitive. Because MCP has no interactive confirmation, an
// unknown name fails with the list of valid names, and an AMBIGUOUS name (more
// than one entity matches) is refused rather than silently mutating the first
// match — the wrong VK revoked or provider disabled is not recoverable by undo.

// resolveProviderID maps a provider name or display name to its id + the display
// label to echo back. Ambiguous matches are refused.
func resolveProviderID(ctx context.Context, gw Gateway, name string) (string, string, error) {
	res, err := gw.Providers(ctx)
	if err != nil {
		return "", "", err
	}
	name = strings.TrimSpace(name)
	var known []string
	var id, label string
	matches := 0
	for _, p := range res.Data {
		lbl := p.DisplayName
		if lbl == "" {
			lbl = p.Name
		}
		known = append(known, lbl)
		if strings.EqualFold(p.Name, name) || strings.EqualFold(p.DisplayName, name) {
			id, label = p.ID, lbl
			matches++
		}
	}
	switch matches {
	case 1:
		return id, label, nil
	case 0:
		return "", "", fmt.Errorf("no provider named %q; known providers: %s", name, strings.Join(known, ", "))
	default:
		return "", "", fmt.Errorf("provider name %q is ambiguous (%d matches) — disable it from the interactive console instead", name, matches)
	}
}

// resolveRuleID maps a routing-rule name to its id. Ambiguous names are refused
// (routing-rule names are not unique-constrained).
func resolveRuleID(ctx context.Context, gw Gateway, name string) (string, string, error) {
	rules, err := gw.RoutingRules(ctx)
	if err != nil {
		return "", "", err
	}
	name = strings.TrimSpace(name)
	var known []string
	var id, label string
	matches := 0
	for _, r := range rules {
		known = append(known, r.Name)
		if strings.EqualFold(r.Name, name) {
			id, label = r.ID, r.Name
			matches++
		}
	}
	switch matches {
	case 1:
		return id, label, nil
	case 0:
		return "", "", fmt.Errorf("no routing rule named %q; known rules: %s", name, strings.Join(known, ", "))
	default:
		return "", "", fmt.Errorf("routing rule name %q is ambiguous (%d matches) — toggle it from the interactive console instead", name, matches)
	}
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
	var id, name, prefix, status string
	revocable := false
	matches := 0
	for _, v := range vks {
		if strings.EqualFold(v.Name, ref) || strings.EqualFold(v.KeyPrefix, ref) || v.ID == ref {
			id, name, prefix, status, revocable = v.ID, v.Name, v.KeyPrefix, v.Status(), v.Revocable()
			matches++
		}
	}
	switch matches {
	case 1:
		if !revocable {
			return "", "", fmt.Errorf("virtual key %q is %s, not active — only active keys can be revoked", ref, status)
		}
		label := name
		if label == "" {
			label = prefix
		}
		return id, label, nil
	case 0:
		return "", "", fmt.Errorf("no virtual key matching %q (by name, key prefix, or id)", ref)
	default:
		return "", "", fmt.Errorf("virtual key reference %q is ambiguous (%d matches) — revoke it from the interactive console instead", ref, matches)
	}
}
