package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

// runResult is the captured outcome of a shell command.
type runResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// runner executes a shell command line and captures its output. A seam so the
// run_command tool is unit-testable without spawning real processes.
type runner interface {
	Run(ctx context.Context, command string) (runResult, error)
}

// osRunner runs the command line through the system shell (`sh -c`).
type osRunner struct{}

func newOSRunner() *osRunner { return &osRunner{} }

func (osRunner) Run(ctx context.Context, command string) (runResult, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	rr := runResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			rr.ExitCode = ee.ExitCode()
			return rr, nil // a non-zero exit is data, not a Go error
		}
		return rr, err // spawn failure (shell missing, etc.)
	}
	return rr, nil
}

// systemTools builds the run_command + read_file + write_file tools. They are
// auto-tier; the kernel Gate's CommandClassifier escalates dangerous commands /
// system-path writes to a confirm. The tool names MUST match the classifier's
// commandToolNames/fileWriteToolNames ("run_command"/"write_file").
func systemTools(r runner) []agent.Tool {
	return []agent.Tool{
		&funcTool{name: "run_command", tier: agent.TierAuto,
			schema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
			desc:   "Run a shell command and return its output. Everyday commands run immediately; destructive ones require operator authorization.",
			run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Command string `json:"command"`
				}
				_ = json.Unmarshal(in, &a)
				if a.Command == "" {
					return errResult("command is required"), nil
				}
				out, err := r.Run(ctx, a.Command)
				if err != nil {
					return errResult("could not run command: %s", err), nil
				}
				if out.ExitCode != 0 {
					return agent.Result{Content: fmt.Sprintf("exit %d\nstdout:\n%s\nstderr:\n%s", out.ExitCode, out.Stdout, out.Stderr), IsError: true}, nil
				}
				body := out.Stdout
				if out.Stderr != "" {
					body += "\nstderr:\n" + out.Stderr
				}
				return agent.Result{Content: body}, nil
			}},

		&funcTool{name: "read_file", tier: agent.TierAuto,
			schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			desc:   "Read a file from disk and return its contents.",
			run: func(_ context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Path string `json:"path"`
				}
				_ = json.Unmarshal(in, &a)
				if a.Path == "" {
					return errResult("path is required"), nil
				}
				b, err := os.ReadFile(a.Path)
				if err != nil {
					return errResult("could not read %q: %s", a.Path, err), nil
				}
				return agent.Result{Content: string(b)}, nil
			}},

		&funcTool{name: "write_file", tier: agent.TierAuto,
			schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
			desc:   "Write content to a file (creating it if needed). Writes to system paths require operator authorization.",
			run: func(_ context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Path    string `json:"path"`
					Content string `json:"content"`
				}
				_ = json.Unmarshal(in, &a)
				if a.Path == "" {
					return errResult("path is required"), nil
				}
				if err := os.WriteFile(a.Path, []byte(a.Content), 0o644); err != nil {
					return errResult("could not write %q: %s", a.Path, err), nil
				}
				return agent.Result{Content: fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.Path)}, nil
			}},
	}
}
