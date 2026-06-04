package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCommandCapturesOutput(t *testing.T) {
	r := &fakeRunner{stdout: "hello\n"}
	rc := toolByName(systemTools(r), "run_command")
	res, err := rc.Run(context.Background(), rawArgs(map[string]any{"command": "echo hello"}))
	if err != nil || res.IsError {
		t.Fatalf("benign command should succeed, got %+v err %v", res, err)
	}
	if !strings.Contains(res.Content, "hello") || r.gotCommand != "echo hello" {
		t.Fatalf("run_command must run the command and capture stdout, got content=%q cmd=%q", res.Content, r.gotCommand)
	}
}

func TestRunCommandStderrAppendedOnSuccess(t *testing.T) {
	r := &fakeRunner{stdout: "out", stderr: "warn"}
	res, _ := toolByName(systemTools(r), "run_command").Run(context.Background(), rawArgs(map[string]any{"command": "x"}))
	if res.IsError || !strings.Contains(res.Content, "out") || !strings.Contains(res.Content, "warn") {
		t.Fatalf("a zero-exit command with stderr must include both streams, got %+v", res)
	}
}

func TestRunCommandNonZeroExitIsErrorResult(t *testing.T) {
	r := &fakeRunner{stderr: "boom", exit: 2}
	res, err := toolByName(systemTools(r), "run_command").Run(context.Background(), rawArgs(map[string]any{"command": "false"}))
	if err != nil {
		t.Fatal("a non-zero exit is a recoverable tool result, not a Go error")
	}
	if !res.IsError || !strings.Contains(res.Content, "exit 2") || !strings.Contains(res.Content, "boom") {
		t.Fatalf("non-zero exit must surface code + stderr, got %+v", res)
	}
}

func TestRunCommandSpawnFailureIsErrorResult(t *testing.T) {
	r := &fakeRunner{err: errors.New("sh not found")}
	res, err := toolByName(systemTools(r), "run_command").Run(context.Background(), rawArgs(map[string]any{"command": "x"}))
	if err != nil || !res.IsError || !strings.Contains(res.Content, "sh not found") {
		t.Fatalf("a spawn failure must surface as an error result, got %+v err %v", res, err)
	}
}

func TestRunCommandRequiresCommand(t *testing.T) {
	res, _ := toolByName(systemTools(&fakeRunner{}), "run_command").Run(context.Background(), json.RawMessage(`{}`))
	if !res.IsError || !strings.Contains(res.Content, "command is required") {
		t.Fatalf("run_command must require a command, got %+v", res)
	}
}

func TestReadWriteFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	tools := systemTools(&fakeRunner{})

	wres, err := toolByName(tools, "write_file").Run(context.Background(), rawArgs(map[string]any{"path": path, "content": "p95 is 90ms"}))
	if err != nil || wres.IsError {
		t.Fatalf("write_file should succeed, got %+v err %v", wres, err)
	}
	if !strings.Contains(wres.Content, "11 bytes") {
		t.Fatalf("write_file must report bytes written, got %q", wres.Content)
	}
	if b, _ := os.ReadFile(path); string(b) != "p95 is 90ms" {
		t.Fatalf("write_file must persist content, file holds %q", string(b))
	}
	rres, err := toolByName(tools, "read_file").Run(context.Background(), rawArgs(map[string]any{"path": path}))
	if err != nil || rres.IsError || rres.Content != "p95 is 90ms" {
		t.Fatalf("read_file must return the content, got %+v err %v", rres, err)
	}
}

func TestReadWriteFileRequirePath(t *testing.T) {
	tools := systemTools(&fakeRunner{})
	if res, _ := toolByName(tools, "read_file").Run(context.Background(), json.RawMessage(`{}`)); !res.IsError || !strings.Contains(res.Content, "path is required") {
		t.Fatalf("read_file must require a path, got %+v", res)
	}
	if res, _ := toolByName(tools, "write_file").Run(context.Background(), rawArgs(map[string]any{"content": "x"})); !res.IsError || !strings.Contains(res.Content, "path is required") {
		t.Fatalf("write_file must require a path, got %+v", res)
	}
}

func TestReadFileMissingIsErrorResult(t *testing.T) {
	res, err := toolByName(systemTools(&fakeRunner{}), "read_file").Run(context.Background(), rawArgs(map[string]any{"path": "/no/such/file/xyz"}))
	if err != nil {
		t.Fatal("a missing file is a recoverable tool result")
	}
	if !res.IsError {
		t.Fatalf("missing file must be an error result, got %+v", res)
	}
}

func TestWriteFileUnwritablePathIsErrorResult(t *testing.T) {
	// A path whose parent does not exist cannot be written.
	res, _ := toolByName(systemTools(&fakeRunner{}), "write_file").Run(context.Background(), rawArgs(map[string]any{"path": "/no/such/dir/out.txt", "content": "x"}))
	if !res.IsError || !strings.Contains(res.Content, "could not write") {
		t.Fatalf("an unwritable path must surface as an error result, got %+v", res)
	}
}

func TestRealRunnerRunsHarmlessCommand(t *testing.T) {
	// Exercise the real os/exec path once with a portable, side-effect-free command.
	res, err := toolByName(systemTools(newOSRunner()), "run_command").Run(context.Background(), rawArgs(map[string]any{"command": "echo nexus-ok"}))
	if err != nil || res.IsError {
		t.Fatalf("real runner on a harmless command should succeed, got %+v err %v", res, err)
	}
	if !strings.Contains(res.Content, "nexus-ok") {
		t.Fatalf("echo should produce output, got %q", res.Content)
	}
}

func TestRealRunnerNonZeroExit(t *testing.T) {
	// The real runner must classify a non-zero exit as data (error result), not a Go error.
	res, err := toolByName(systemTools(newOSRunner()), "run_command").Run(context.Background(), rawArgs(map[string]any{"command": "exit 3"}))
	if err != nil {
		t.Fatalf("non-zero exit must not be a Go error, got %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "exit 3") {
		t.Fatalf("real runner must report the exit code, got %+v", res)
	}
}
