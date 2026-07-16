package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bruce-go/internal/approval"
	"bruce-go/internal/llm"
	"bruce-go/internal/runtime"
	"bruce-go/internal/sandbox"
)

func TestRegistryBuiltinsAndHITL(t *testing.T) {
	dir := t.TempDir()
	registry := NewRegistry(dir)

	out := registry.Execute(context.Background(), "write_file", map[string]string{"path": "a.txt", "content": "hello"})
	if !strings.Contains(out, "文件已写入") {
		t.Fatalf("write_file output = %q", out)
	}
	out = registry.Execute(context.Background(), "edit_file", map[string]string{"path": "a.txt", "old_text": "hello", "new_text": "hi"})
	if !strings.Contains(out, "文件已编辑") {
		t.Fatalf("edit_file output = %q", out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hi" {
		t.Fatalf("file content = %q", data)
	}
	out = registry.Execute(context.Background(), "read_file", map[string]string{"path": "a.txt"})
	if !strings.Contains(out, "hi") {
		t.Fatalf("read_file output = %q", out)
	}

	registry.WithHITL(approval.NewAutoHandler(true, approval.Reject("no writes")))
	out = registry.Execute(context.Background(), "write_file", map[string]string{"path": "b.txt", "content": "blocked"})
	if !strings.Contains(out, "[HITL] 操作已被拒绝") {
		t.Fatalf("HITL output = %q", out)
	}
}

func TestHITLModifiedArgumentsAreRevalidated(t *testing.T) {
	dir := t.TempDir()
	modifiedWrite := approval.Result{Decision: approval.Modified, Arguments: `{"path":".git/config","content":"malicious"}`}
	registry := NewRegistry(dir).WithHITL(approval.NewAutoHandler(true, modifiedWrite))
	out := registry.Execute(context.Background(), "write_file", map[string]string{"path": "safe.txt", "content": "safe"})
	if !strings.Contains(out, "文件工具禁止直接修改 .git") {
		t.Fatalf("modified write was not revalidated: %q", out)
	}

	modifiedCommand := approval.Result{Decision: approval.Modified, Arguments: `{"command":"rm -rf /"}`}
	registry.WithHITL(approval.NewAutoHandler(true, modifiedCommand))
	out = registry.Execute(context.Background(), "execute_command", map[string]string{"command": "pwd"})
	if !strings.Contains(out, "命令被安全策略拒绝") {
		t.Fatalf("modified command was not revalidated: %q", out)
	}
}

func TestCommandGuardRejectsDangerousCommands(t *testing.T) {
	tests := []string{"rm -rf /", "sudo reboot", "find / -name '*.go'", "grep -R foo /", "ls -R /"}
	for _, command := range tests {
		if got := (CommandGuard{}).Check(command); got.Allowed {
			t.Fatalf("command %q should be rejected", command)
		}
	}
	if got := (CommandGuard{}).Check("find . -name '*.go'"); !got.Allowed {
		t.Fatalf("workspace scan should be allowed: %+v", got)
	}
}

func TestBuildPromptIncludesRoutingGuidelines(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	prompt := registry.BuildPrompt()
	for _, want := range []string{"Guidelines:", "rg --files", "读取已知路径的单个文件用 read_file", "小范围修改已有文件用 edit_file", "新建文件或完整覆盖文件用 write_file"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}

	registry.Register(Tool{
		Name:          "mcp__filesystem__directory_tree",
		Description:   "directory tree",
		Parameters:    mustSchema(t, `{"type":"object","properties":{}}`),
		Exec:          func(context.Context, map[string]string) (string, error) { return "[]", nil },
		PromptSnippet: "MCP filesystem tree",
	})
	prompt = registry.BuildPrompt()
	if !strings.Contains(prompt, "mcp__* 只在用户明确要求 MCP") {
		t.Fatalf("prompt missing MCP guidance:\n%s", prompt)
	}
}

func TestReadFileOffsetLimitAndContinuationHints(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("line1\nline2\nline3\nline4"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(dir)
	out := registry.Execute(context.Background(), "read_file", map[string]string{"path": "notes.txt", "offset": "2", "limit": "2"})
	for _, want := range []string{"lines 2-3 of 4", "line2\nline3", "Use offset=4 to continue"} {
		if !strings.Contains(out, want) {
			t.Fatalf("read_file output missing %q:\n%s", want, out)
		}
	}
	out = registry.Execute(context.Background(), "read_file", map[string]string{"path": "notes.txt", "offset": "5"})
	if !strings.Contains(out, "offset 超出文件末尾") || !strings.Contains(out, "文件总行数=4") {
		t.Fatalf("offset output = %q", out)
	}
}

func TestReadFileLargeOutputKeepsContinuationHint(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < 500; i++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", 40))
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "large.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(dir)
	out := registry.Execute(context.Background(), "read_file", map[string]string{"path": "large.txt"})
	if !strings.Contains(out, "char limit") || !strings.Contains(out, "Use offset=") {
		t.Fatalf("large read output missing continuation hint:\n%s", out)
	}
}

func TestFileToolsRejectTraversalSymlinksAndGitMetadata(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(workspace, "final-link")); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(workspace)

	out := registry.Execute(context.Background(), "read_file", map[string]string{"path": "../secret.txt"})
	if !strings.Contains(out, "路径超出工作目录") {
		t.Fatalf("parent traversal output = %q", out)
	}
	out = registry.Execute(context.Background(), "read_file", map[string]string{"path": filepath.Join(outside, "secret.txt")})
	if !strings.Contains(out, "路径超出工作目录") {
		t.Fatalf("absolute traversal output = %q", out)
	}
	out = registry.Execute(context.Background(), "read_file", map[string]string{"path": "escape/secret.txt"})
	if strings.Contains(out, "outside-secret") || !strings.Contains(out, "工具执行失败") {
		t.Fatalf("symlink read output = %q", out)
	}
	out = registry.Execute(context.Background(), "write_file", map[string]string{"path": "escape/new.txt", "content": "bad"})
	if !strings.Contains(out, "工具执行失败") {
		t.Fatalf("symlink write output = %q", out)
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("symlink write escaped workspace: %v", err)
	}
	out = registry.Execute(context.Background(), "read_file", map[string]string{"path": "final-link"})
	if strings.Contains(out, "outside-secret") || !strings.Contains(out, "工具执行失败") {
		t.Fatalf("final symlink read output = %q", out)
	}
	out = registry.Execute(context.Background(), "read_file", map[string]string{"path": "escape/"})
	if strings.Contains(out, "outside-secret") || !strings.Contains(out, "工具执行失败") {
		t.Fatalf("trailing slash escape output = %q", out)
	}
	out = registry.Execute(context.Background(), "write_file", map[string]string{"path": ".git/config", "content": "bad"})
	if !strings.Contains(out, "禁止直接修改 .git") {
		t.Fatalf("git metadata output = %q", out)
	}
}

func TestReadOnlySandboxRejectsFileWritesBeforeExecution(t *testing.T) {
	workspace := t.TempDir()
	manager, err := sandbox.New(context.Background(), sandbox.Options{Workspace: workspace, HomeDir: t.TempDir(), Mode: sandbox.ModeReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	registry := NewRegistry(workspace).WithSandbox(manager)
	out := registry.Execute(context.Background(), "write_file", map[string]string{"path": "blocked.txt", "content": "bad"})
	if !strings.Contains(out, "read-only 模式") {
		t.Fatalf("read-only output = %q", out)
	}
}

func TestParallelToolCallExecutor(t *testing.T) {
	registry := EmptyRegistry(t.TempDir())
	var running int32
	var maxSeen int32
	registry.Register(Tool{
		Name:        "sleepy",
		Description: "sleepy fake tool",
		Parameters:  mustSchema(t, `{"type":"object","properties":{"value":{"type":"string"}}}`),
		Exec: func(_ context.Context, args map[string]string) (string, error) {
			cur := atomic.AddInt32(&running, 1)
			for {
				old := atomic.LoadInt32(&maxSeen)
				if cur <= old || atomic.CompareAndSwapInt32(&maxSeen, old, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&running, -1)
			return args["value"], nil
		},
	})
	calls := []llm.ToolCall{
		{ID: "1", Function: llm.FunctionCall{Name: "sleepy", Arguments: `{"value":"a"}`}},
		{ID: "2", Function: llm.FunctionCall{Name: "sleepy", Arguments: `{"value":"b"}`}},
	}
	executor := ParallelExecutor{Registry: registry, Config: runtime.ConcurrencyConfig{MaxParallelism: 2, BatchTimeout: time.Second, MaxOutputChars: 1000}}
	results := executor.Execute(context.Background(), calls)
	if len(results) != 2 || results[0].Result != "a" || results[1].Result != "b" {
		t.Fatalf("results = %+v", results)
	}
	if atomic.LoadInt32(&maxSeen) < 2 {
		t.Fatalf("expected parallel execution, maxSeen=%d", maxSeen)
	}
}

func mustSchema(t *testing.T, raw string) json.RawMessage {
	t.Helper()
	var data json.RawMessage = []byte(raw)
	return data
}

func TestParseToolResultExtractsImageBlocks(t *testing.T) {
	// A small valid PNG base64 string (1x1 red pixel)
	raw := "text before\n[bruce-image-content mimeType=image/png source=test]\niVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8/5+hHgAHggJ/PchI7wAAAABJRU5ErkJggg==\n[/bruce-image-content]\ntext after"
	clean, parts := ParseToolResult(raw)
	if !strings.Contains(clean, "text before") || !strings.Contains(clean, "text after") {
		t.Fatalf("clean text = %q", clean)
	}
	if len(parts) != 1 {
		t.Fatalf("expected 1 image part, got %d", len(parts))
	}
	if parts[0].Type != llm.ContentImageURL {
		t.Fatalf("expected image part, got %+v", parts[0])
	}
}

func TestParseToolResultNoImages(t *testing.T) {
	raw := "plain text only"
	clean, parts := ParseToolResult(raw)
	if clean != "plain text only" {
		t.Fatalf("clean = %q", clean)
	}
	if len(parts) != 0 {
		t.Fatalf("expected 0 parts, got %d", len(parts))
	}
}

func TestEncodeToolImage(t *testing.T) {
	out := EncodeToolImage("image/png", "dGVzdA==", "test-source")
	if !strings.Contains(out, "[bruce-image-content mimeType=image/png source=test-source]") {
		t.Fatalf("output = %q", out)
	}
	if !strings.Contains(out, "dGVzdA==") {
		t.Fatalf("output missing base64 data: %q", out)
	}
}
