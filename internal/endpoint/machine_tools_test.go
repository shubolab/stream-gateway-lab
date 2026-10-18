package endpoint

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func machineCall(t *testing.T, ctx context.Context, name string, args map[string]any) map[string]any {
	t.Helper()
	input, _ := json.Marshal(args)
	output, err := ExecuteTool(ctx, name, input)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestMachineFilesFullAccessAndEdit(t *testing.T) {
	workdir, outside := t.TempDir(), t.TempDir()
	file := filepath.Join(outside, "nested", "script.py")
	machineCall(t, context.Background(), "fs_write", map[string]any{"workdir": workdir, "path": file, "content": "one\ntwo\ntwo\n"})
	input, _ := json.Marshal(map[string]any{"workdir": workdir, "path": file, "old_string": "two", "new_string": "fixed"})
	if _, err := ExecuteTool(context.Background(), "fs_edit", input); err == nil {
		t.Fatal("ambiguous edit must not modify the file")
	}
	data, _ := os.ReadFile(file)
	if string(data) != "one\ntwo\ntwo\n" {
		t.Fatal("failed edit changed file")
	}
	machineCall(t, context.Background(), "fs_edit", map[string]any{"workdir": workdir, "path": file, "old_string": "two", "new_string": "fixed", "replace_all": true})
	r := machineCall(t, context.Background(), "fs_read", map[string]any{"workdir": workdir, "path": file, "offset": 1, "limit": 1})
	if r["content"] != "fixed\n" || r["next_offset"] != float64(2) {
		t.Fatalf("incorrect read window: %v", r)
	}
	r = machineCall(t, context.Background(), "fs_glob", map[string]any{"workdir": outside, "pattern": "**/*.py"})
	if len(r["items"].([]any)) != 1 {
		t.Fatalf("nested glob: %v", r)
	}
	r = machineCall(t, context.Background(), "fs_grep", map[string]any{"workdir": outside, "pattern": "fixed", "glob": "*.py"})
	if len(r["items"].([]any)) != 2 {
		t.Fatalf("grep: %v", r)
	}
}

func TestMachineCommandFailureAndTimeout(t *testing.T) {
	dir := t.TempDir()
	r := machineCall(t, context.Background(), "shell_exec", map[string]any{"workdir": dir, "command": "echo observed-error >&2; exit 7"})
	if r["exit_code"] != float64(7) || !strings.Contains(r["stderr"].(string), "observed-error") {
		t.Fatalf("nonzero exit must be an observation: %v", r)
	}
	start := time.Now()
	r = machineCall(t, context.Background(), "shell_exec", map[string]any{"workdir": dir, "command": "sleep 20", "timeout_ms": 100})
	if r["timed_out"] != true || time.Since(start) > 2*time.Second {
		t.Fatalf("command timeout failed: %v", r)
	}
	r = machineCall(t, context.Background(), "shell_exec", map[string]any{"workdir": dir, "command": "yes output | head -c 50000"})
	if r["truncated"] != true || len(r["stdout"].(string)) != 4096 {
		t.Fatalf("output not bounded: length=%d truncated=%v", len(r["stdout"].(string)), r["truncated"])
	}
}

func TestMachineReadOversizedLine(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "long"), []byte(strings.Repeat("x", 9000)), 0600); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]any{"workdir": dir, "path": "long"})
	if _, err := ExecuteTool(context.Background(), "fs_read", input); err == nil {
		t.Fatal("oversized line should return a recoverable error")
	}
}
