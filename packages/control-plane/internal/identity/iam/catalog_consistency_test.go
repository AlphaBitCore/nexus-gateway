// Package iam — cross-layer consistency gates.
//
// The four tests in this file scan the source tree to prove that the
// canonical taxonomy boundary (packages/shared/identity/iam.Catalog) is enforced
// at every boundary the SDD lists in docs/developers/specs/e43-s1-iam-canonical-
// taxonomy.md §3.6:
//
//  1. iamMW route guards never carry raw "admin:..." string literals;
//     every guard derives its action from the catalog. Caught by:
//     TestNoRawAdminActionStringsInHandlerLiterals.
//
//  2. Audit constructions never set ae.Action / ae.EntityType from
//     string literals; every audit Entry comes from audit.EntryFor.
//     Caught by: TestNoFreeFormAuditAssignments.
//
//  3. Every `admin:...` string anywhere under the scanned domain trees
//     matches the canonical regex (no surviving admin:CamelCase, no
//     phantom strings introduced in new code). Caught by:
//     TestAllAdminActionStringsAreCanonical.
//
//  4. Every `admin:...` string in the seed (tools/db-migrate/seed/seed.ts)
//     matches the canonical regex. Caught by: TestSeededPoliciesUseCanonicalActions.
//
// Walk roots: handler/ is the historical home of admin endpoints, but
// post-decomp the same patterns landed under observability/, governance/,
// fleet/, settings/, identity/sso, ai/. All these trees ship admin
// handlers + register iamMW routes + emit audit entries, so they must be
// covered by the same gates. handlerWalkRoots is the single source of
// truth — keep it aligned with packages/control-plane/internal/<dir>/
// growth.
//
// All four tests fail BUILD if violated, so any future regression
// surfaces in CI rather than at runtime.
package iam

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot returns the path to the repository root by walking up from
// the test binary's working directory until it finds tools/db-migrate.
// Used to resolve the absolute paths of handler/ and seed.ts since this
// test file's CWD is `packages/control-plane/internal/iam`.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for d := wd; d != "/" && d != "."; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "tools", "db-migrate")); err == nil {
			return d
		}
	}
	t.Fatalf("could not find repo root from %s", wd)
	return ""
}

// handlerWalkRoots is the canonical set of `packages/control-plane/internal/<sub>`
// trees that the canonical-action / audit-assignment gates scan. handler/
// is the original home; the rest are sibling domain subtrees that
// register admin routes via iamMW and emit audit entries via audit.EntryFor
// after the 2026 directory decomp. Adding a new domain that ships admin
// handlers MUST extend this slice — otherwise the canonical-taxonomy
// invariants stop catching regressions in the new tree.
var handlerWalkRoots = []string{
	"packages/control-plane/internal/handler",
	"packages/control-plane/internal/observability",
	"packages/control-plane/internal/governance",
	"packages/control-plane/internal/fleet",
	"packages/control-plane/internal/settings",
	"packages/control-plane/internal/ai",
	"packages/control-plane/internal/identity/sso",
	"packages/control-plane/internal/infrastructure",
	"packages/control-plane/internal/traffic",
}

func walkGoSources(root, dir string, t *testing.T) []string {
	t.Helper()
	var out []string
	target := filepath.Join(root, dir)
	if _, err := os.Stat(target); os.IsNotExist(err) {
		// A walk root may be removed in a future decomp without us noticing;
		// fail loudly so handlerWalkRoots stays honest rather than silently
		// shrinking the scan surface.
		t.Fatalf("walk root %s does not exist; remove from handlerWalkRoots if intentional", target)
	}
	err := filepath.Walk(target, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// walkAllHandlerRoots returns every Go source file across every entry in
// handlerWalkRoots. Used by the consistency gates to scan all admin-
// handler-bearing trees with one helper call.
func walkAllHandlerRoots(root string, t *testing.T) []string {
	t.Helper()
	var all []string
	for _, sub := range handlerWalkRoots {
		all = append(all, walkGoSources(root, sub, t)...)
	}
	return all
}

// TestNoRawAdminActionStringsInHandlerLiterals fails on any
// iamMW("admin:...") call where the argument is a raw string literal.
// Every iamMW must receive an expression that resolves via
// the catalog (typically iam.ResourceX.Action(iam.VerbY)).
func TestNoRawAdminActionStringsInHandlerLiterals(t *testing.T) {
	root := repoRoot(t)
	// Match `iamMW("admin:...")` — i.e. the iamMW call with a quoted
	// string argument that starts with the admin: prefix.
	re := regexp.MustCompile(`iamMW\("admin:`)
	var hits []string
	for _, f := range walkAllHandlerRoots(root, t) {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				hits = append(hits, formatHit(f, i+1, line, root))
			}
		}
	}
	if len(hits) > 0 {
		t.Fatalf("found %d iamMW call(s) with raw \"admin:...\" string literals; use iam.ResourceX.Action(iam.VerbY) instead:\n%s",
			len(hits), strings.Join(hits, "\n"))
	}
}

// TestNoFreeFormAuditAssignments fails on any direct ae.Action / ae.EntityType
// assignment in handler code. The only allowed construction
// path for an audit Entry that carries (Action, EntityType) is
// audit.EntryFor(c, resource, verb). Free-form assignment historically
// produced the virtualKey/virtual_key + camelCase/snake_case drift this
// epic is fixing.
//
// The test scans for `<varname>.Action\s*=\s*"` and same for EntityType.
// `varname` is constrained to {ae, rae, e} to avoid false positives on
// unrelated structs (hub.ConfigChangeRequest.Action, IamPolicy
// Statement.Action, etc.). Adding a new audit variable name requires
// adding it here.
func TestNoFreeFormAuditAssignments(t *testing.T) {
	root := repoRoot(t)
	auditVars := []string{"ae", "rae", "e"}
	patterns := make([]*regexp.Regexp, 0, len(auditVars)*2)
	for _, v := range auditVars {
		patterns = append(patterns,
			regexp.MustCompile(`\b`+regexp.QuoteMeta(v)+`\.Action\s*=\s*"`),
			regexp.MustCompile(`\b`+regexp.QuoteMeta(v)+`\.EntityType\s*=\s*"`),
		)
	}

	// auth_sessions.go has one direct audit.Entry{...} literal in the
	// Hub-driven internal revoke-device path; actor identity there is
	// synthetic so EntryFor (which reads admin auth from echo.Context)
	// is intentionally bypassed. The struct literal uses
	// iam.ResourceNexusSession.Name + iam.VerbRevoke for its field
	// values, so the canonical invariant still holds; we just allow the
	// literal pattern in that one file.
	allowedFiles := map[string]bool{
		"auth_sessions.go": true,
	}

	var hits []string
	for _, f := range walkAllHandlerRoots(root, t) {
		if allowedFiles[filepath.Base(f)] {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, re := range patterns {
				if re.MatchString(line) {
					hits = append(hits, formatHit(f, i+1, line, root))
					break
				}
			}
		}
	}
	if len(hits) > 0 {
		t.Fatalf("found %d ae.Action= / ae.EntityType= literal assignment(s); use audit.EntryFor(c, iam.ResourceX, iam.VerbY) instead:\n%s",
			len(hits), strings.Join(hits, "\n"))
	}
}

// TestAllAdminActionStringsAreCanonical fails on any "admin:..." string
// literal under handler/ whose body does not match the canonical
// admin:<resource>.<verb> shape. Catches future regressions where
// somebody hand-types admin:CamelCase or admin:something-phantom.
//
// Strings inside iamMW() are already filtered by test #1; this test is
// a wider safety net for any other admin: string that might land in
// handler code (audit.Entry{Action: "admin:..."} construction, error
// messages, comments are excluded, etc.).
func TestAllAdminActionStringsAreCanonical(t *testing.T) {
	root := repoRoot(t)
	// Find every quoted admin: string. Allow admin:* and
	// admin:<glob>.* / admin:*.<verb> wildcard patterns too.
	stringRe := regexp.MustCompile(`"admin:[^"]+"`)
	canonicalRe := regexp.MustCompile(`^"admin:(\*|[a-z][a-z0-9-]*(\.\*|\.[a-z][a-z-]*)|\*\.[a-z][a-z-]*)"$`)

	var hits []string
	for _, f := range walkAllHandlerRoots(root, t) {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		// Strip Go single-line comments so admin:Camel inside a //
		// comment doesn't trip the test (commit comments reference
		// pre-migration action names for explanatory purposes).
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if idx := strings.Index(line, "//"); idx >= 0 {
				line = line[:idx]
			}
			for _, m := range stringRe.FindAllString(line, -1) {
				if !canonicalRe.MatchString(m) {
					hits = append(hits, formatHit(f, i+1, strings.TrimSpace(line), root))
				}
			}
		}
	}
	if len(hits) > 0 {
		t.Fatalf("found %d non-canonical admin:... string(s) under control-plane/internal/*; expect admin:<resource>.<verb> per shared/iam.Catalog:\n%s",
			len(hits), strings.Join(hits, "\n"))
	}
}

// TestSeededPoliciesUseCanonicalActions scans the Prisma seed file for
// `admin:...` strings and asserts every one matches the canonical
// regex. Production-equivalent of test #3 for the seed surface; would
// have failed on the pre-migration seed.ts block 13 with its 10+ phantom
// action references that block 13's deletion (P3) removed.
//
// Implementation: regex-scan the seed.ts source file rather than parse
// TypeScript. The seed file uses single-line quoted literals
// consistently so a regex is sufficient.
func TestSeededPoliciesUseCanonicalActions(t *testing.T) {
	root := repoRoot(t)
	seedPath := filepath.Join(root, "tools", "db-migrate", "seed", "seed.ts")
	data, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatalf("read seed.ts: %v", err)
	}

	// TypeScript uses single quotes — the legacy block 13 used
	// 'admin:CreateProvider', and the canonical block 15c uses
	// 'admin:provider.create'.
	stringRe := regexp.MustCompile(`'admin:[^']+'`)
	canonicalRe := regexp.MustCompile(`^'admin:(\*|[a-z][a-z0-9-]*(\.\*|\.[a-z][a-z-]*)|\*\.[a-z][a-z-]*)'$`)

	var hits []string
	for i, line := range strings.Split(string(data), "\n") {
		// Skip block-13 references in comments left as breadcrumbs.
		stripped := line
		if idx := strings.Index(stripped, "//"); idx >= 0 {
			stripped = stripped[:idx]
		}
		for _, m := range stringRe.FindAllString(stripped, -1) {
			if !canonicalRe.MatchString(m) {
				hits = append(hits, formatHit(seedPath, i+1, strings.TrimSpace(stripped), root))
			}
		}
	}
	if len(hits) > 0 {
		t.Fatalf("found %d non-canonical admin: action(s) in tools/db-migrate/seed/seed.ts; expect admin:<resource>.<verb>:\n%s",
			len(hits), strings.Join(hits, "\n"))
	}
}

func formatHit(path string, line int, content, root string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	return "  " + rel + ":" + itoa(line) + "  " + content
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	if n < 0 {
		digits = append(digits, '-')
		n = -n
	}
	var rev []byte
	for n > 0 {
		rev = append(rev, byte('0'+n%10))
		n /= 10
	}
	for i := len(rev) - 1; i >= 0; i-- {
		digits = append(digits, rev[i])
	}
	return string(digits)
}
