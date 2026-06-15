package agent

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// Decision is the permission outcome computed before a tool runs.
type Decision int

const (
	// Allow runs the tool without prompting.
	Allow Decision = iota
	// Ask requires human authorization (the TUI Allow/Deny confirm gate).
	Ask
)

// Classifier inspects a tool name + raw input and reports whether this specific
// call is dangerous enough to require authorization even when the tool's base
// tier is auto. Implementations are pure.
type Classifier interface {
	Classify(toolName string, input json.RawMessage) (dangerous bool, reason string)
}

// ConfirmDetailer is an optional Tool capability: a confirm-tier tool that can
// describe, from the concrete input, exactly what its write will do — e.g. the
// resolved "METHOD /path (operationId)" or the named entity action. The gate uses
// it as the Ask reason so the operator authorizes an informed change, not a generic
// "mitigation". Defined here to keep the kernel pure; capabilities implements it.
type ConfirmDetailer interface {
	ConfirmDetail(input json.RawMessage) string
}

// ImpactDetailer is an optional Tool capability: a high-blast-radius confirm-tier
// tool that can produce a structured, human-facing preview of what executing it
// WOULD change — current state → effect — for display in the confirm card BEFORE the
// operator approves. Unlike ConfirmDetailer (pure, string, called
// in the Gate), ImpactDetail may read current state, so it takes a ctx and returns a
// JSON-serializable value. It MUST NOT mutate anything. A (nil, nil) return means
// "no preview for this tool/input" (the common case); a non-nil error means the
// preview could not be computed — callers fail OPEN (still allow the confirm, with an
// "unavailable" note) so a degraded read never blocks an emergency mitigation.
type ImpactDetailer interface {
	ImpactDetail(ctx context.Context, input json.RawMessage) (any, error)
}

// DynamicTier lets a tool refine its STATIC tier per input: a tool whose risk
// depends on what it is asked to do (running a workflow whose grounded blast
// radius is empty is a read; one that revokes keys is a mitigation) reports
// the tier for THIS call. Implementations must be grounded and fail SAFE —
// when the risk cannot be established, return the static tier (or TierConfirm),
// never TierAuto. The gate consults it before the static Tier().
type DynamicTier interface {
	TierFor(input json.RawMessage) Tier
}

// Gate is the pre-execution permission check. It mirrors Claude Code: auto runs
// immediately, dangerous/confirm calls ask, an operator allowlist pre-approves
// patterns, and a YOLO bypass auto-approves everything.
type Gate struct {
	classifier Classifier
	allowlist  []string
	yolo       bool
}

// NewGate builds a gate. classifier may be nil (then only the static Tier and
// allowlist apply). allowlist entries are substrings matched against the
// command/path extracted from the input.
func NewGate(classifier Classifier, allowlist []string, yolo bool) *Gate {
	return &Gate{classifier: classifier, allowlist: allowlist, yolo: yolo}
}

// Decide returns the permission decision and, when Ask, a human-readable reason.
func (g *Gate) Decide(tool Tool, input json.RawMessage) (Decision, string) {
	if g.yolo {
		return Allow, ""
	}
	tier := tool.Tier()
	if dt, ok := tool.(DynamicTier); ok {
		tier = dt.TierFor(input)
	}
	if tier == TierConfirm {
		if d, ok := tool.(ConfirmDetailer); ok {
			if detail := strings.TrimSpace(d.ConfirmDetail(input)); detail != "" {
				return Ask, detail
			}
		}
		return Ask, "mitigation requires authorization"
	}
	if g.classifier != nil {
		if dangerous, reason := g.classifier.Classify(tool.Name(), input); dangerous {
			if g.allowed(input) {
				return Allow, ""
			}
			return Ask, reason
		}
	}
	return Allow, ""
}

// allowed reports whether the input's command/path matches any allowlist entry.
func (g *Gate) allowed(input json.RawMessage) bool {
	subject := extractSubject(input)
	for _, p := range g.allowlist {
		if p != "" && strings.Contains(subject, p) {
			return true
		}
	}
	return false
}

// dangerCmd patterns give a PRECISE reason for the most common destructive ops.
// They are matched first only for the better message — the fail-safe default
// (see Classify) already requires authorization for anything not known-safe, so
// a missing pattern here can never make a destructive command auto-run.
var dangerCmd = []struct {
	re     *regexp.Regexp
	reason string
}{
	// rm with any recursive/force flag, in any order or fused/split form.
	{regexp.MustCompile(`\brm\b[^;&|]*(\s-[a-zA-Z]*[rRf][a-zA-Z]*|\s--recursive|\s--force)\b`), "recursive/forced delete"},
	// git push --force / -f / +refspec (all rewrite remote history).
	{regexp.MustCompile(`\bgit\s+push\b[^;&|]*(--force|\s-f\b|\s\+\S)`), "force push rewrites history"},
	{regexp.MustCompile(`(?i)\bdrop\s+(table|database|schema)\b`), "drops a database object"},
	{regexp.MustCompile(`(?i)\btruncate\s+table\b`), "truncates a table"},
	{regexp.MustCompile(`\bdd\s+if=`), "raw disk write"},
	{regexp.MustCompile(`\bmkfs\b`), "formats a filesystem"},
	{regexp.MustCompile(`\b(shutdown|reboot|halt)\b`), "halts the host"},
	{regexp.MustCompile(`\bkill(all)?\s+-9\b`), "force-kills processes"},
	{regexp.MustCompile(`>\s*/dev/sd`), "overwrites a block device"},
	{regexp.MustCompile(`\bchmod\s+-R\b`), "recursive permission change"},
	{regexp.MustCompile(`\bchown\s+-R\b`), "recursive ownership change"},
	// find / fd are safe-prefix reads ONLY without an action flag; -delete and the
	// -exec family turn find into a recursive delete/exec engine, and fd's -x/-X/
	// --exec run a command per match — so those forms must require review even
	// though the bare search is auto-allowed (see safePrefixes).
	{regexp.MustCompile(`\bfind\b[^;&|]*\s-(delete|execdir|exec|okdir|ok)\b`), "find deletes or runs a command per match"},
	{regexp.MustCompile(`\bfd(find)?\b[^;&|]*\s(-x|-X|--exec)\b`), "fd runs a command per match"},
}

// shellMeta flags compound / redirecting commands. A command that chains or
// redirects can hide anything, so it is never auto-run (fail-safe).
var shellMeta = regexp.MustCompile("[;&|<>`]" + `|\$\(`)

// safePrefixes are read-only / inspect / build commands that auto-run without a
// prompt. The classifier is an ALLOWLIST: only these (with no shell metachars)
// are safe; everything else requires authorization. This is fail-safe by design
// — the opposite of a leaky denylist — matching how Claude Code gates the shell.
var safePrefixes = []string{
	// find / fd below are read-only here ONLY: their destructive forms (-delete, -exec*,
	// -x) are caught first by dangerCmd, so a bare search auto-runs but an action form asks.
	"ls", "ll", "pwd", "cat", "head", "tail", "wc", "grep", "egrep", "fgrep", "rg", "find", "fd",
	"echo", "printf", "env", "printenv", "date", "cal", "whoami", "id", "hostname", "uname",
	"which", "type", "file", "stat", "du", "df", "free", "tree", "basename", "dirname", "realpath",
	"readlink", "sort", "uniq", "cut", "column", "jq", "yq", "diff", "cmp", "ps", "uptime",
	"go build", "go test", "go vet", "go list", "go version", "go env", "go doc", "go fmt", "gofmt",
	"git status", "git diff", "git log", "git show", "git branch", "git remote", "git rev-parse",
	"git describe", "git fetch", "git stash list",
	"kubectl get", "kubectl describe", "kubectl logs", "kubectl top",
	"docker ps", "docker images", "docker logs",
}

// sensitivePathPrefixes: writing under these is dangerous (system/config dirs).
var sensitivePathPrefixes = []string{"/etc", "/usr", "/bin", "/sbin", "/var", "/System", "/Library", "/boot", "/dev"}

// commandToolNames carry a "command" string; fileWriteToolNames carry a "path".
var commandToolNames = map[string]bool{"run_command": true, "bash": true, "shell": true}
var fileWriteToolNames = map[string]bool{"write_file": true, "edit_file": true}

// CommandClassifier auto-allows only known-safe shell commands and writes to
// ordinary paths; everything else (unknown commands, compound/redirecting
// commands, writes to system paths) requires authorization.
type CommandClassifier struct{}

// NewCommandClassifier returns the default danger classifier.
func NewCommandClassifier() *CommandClassifier { return &CommandClassifier{} }

// Classify implements Classifier.
func (c *CommandClassifier) Classify(toolName string, input json.RawMessage) (bool, string) {
	if commandToolNames[toolName] {
		var v struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(input, &v)
		cmd := strings.TrimSpace(v.Command)
		if cmd == "" {
			return false, ""
		}
		// Precise reason for the common destructive ops.
		for _, d := range dangerCmd {
			if d.re.MatchString(cmd) {
				return true, d.reason
			}
		}
		// Compound / redirecting commands can hide anything → require review.
		if shellMeta.MatchString(cmd) {
			return true, "compound or redirecting command — review before running"
		}
		// Known-safe read/inspect/build commands auto-run.
		if isSafeCommand(cmd) {
			return false, ""
		}
		// Fail-safe: anything unrecognized requires authorization.
		return true, "unrecognized command — review before running"
	}
	if fileWriteToolNames[toolName] {
		var v struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(input, &v)
		clean := strings.TrimSpace(v.Path)
		for _, p := range sensitivePathPrefixes {
			if clean == p || strings.HasPrefix(clean, p+"/") {
				return true, "writes to a system path " + p
			}
		}
		return false, ""
	}
	return false, ""
}

// isSafeCommand reports whether cmd begins with a known-safe prefix.
func isSafeCommand(cmd string) bool {
	for _, p := range safePrefixes {
		if cmd == p || strings.HasPrefix(cmd, p+" ") {
			return true
		}
	}
	return false
}

// extractSubject pulls the command or path string from a tool input for
// allowlist matching.
func extractSubject(input json.RawMessage) string {
	var v struct {
		Command string `json:"command"`
		Path    string `json:"path"`
	}
	_ = json.Unmarshal(input, &v)
	if v.Command != "" {
		return v.Command
	}
	return v.Path
}
