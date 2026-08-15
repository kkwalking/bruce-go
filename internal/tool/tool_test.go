package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	if !strings.Contains(out, "File written") {
		t.Fatalf("write_file output = %q", out)
	}
	out = registry.Execute(context.Background(), "edit_file", map[string]string{"path": "a.txt", "old_text": "hello", "new_text": "hi"})
	if !strings.Contains(out, "File edited") {
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
	if !strings.Contains(out, "[HITL] Operation was rejected") {
		t.Fatalf("HITL output = %q", out)
	}
}

func TestRegistrySubsetKeepsSharedExecutionSettings(t *testing.T) {
	dir := t.TempDir()
	registry := NewRegistry(dir).WithHITL(approval.NewAutoHandler(true, approval.Reject("no writes")))
	subset := registry.Subset("read_file", "write_file")

	names := subset.ToolNames()
	if strings.Join(names, ",") != "read_file,write_file" {
		t.Fatalf("subset tool names = %v", names)
	}
	if _, ok := subset.Lookup("edit_file"); ok {
		t.Fatal("subset unexpectedly contains edit_file")
	}
	if out := subset.Execute(context.Background(), "write_file", map[string]string{"path": "a.txt", "content": "blocked"}); !strings.Contains(out, "[HITL] Operation was rejected") {
		t.Fatalf("subset write did not reuse HITL: %q", out)
	}
	if out := subset.Execute(context.Background(), "edit_file", map[string]string{"path": "a.txt", "old_text": "x", "new_text": "y"}); !strings.Contains(out, "Unknown tool: edit_file") {
		t.Fatalf("subset edit_file output = %q", out)
	}
}

func TestHITLModifiedArgumentsAreRevalidated(t *testing.T) {
	dir := t.TempDir()
	modifiedWrite := approval.Result{Decision: approval.Modified, Arguments: `{"path":".git/config","content":"malicious"}`}
	registry := NewRegistry(dir).WithHITL(approval.NewAutoHandler(true, modifiedWrite))
	out := registry.Execute(context.Background(), "write_file", map[string]string{"path": "safe.txt", "content": "safe"})
	if !strings.Contains(out, "file tools must not modify .git directly") {
		t.Fatalf("modified write was not revalidated: %q", out)
	}

	modifiedCommand := approval.Result{Decision: approval.Modified, Arguments: `{"command":"rm -rf /"}`}
	registry.WithHITL(approval.NewAutoHandler(true, modifiedCommand))
	out = registry.Execute(context.Background(), "execute_command", map[string]string{"command": "pwd"})
	if !strings.Contains(out, "Command rejected by security policy") {
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
	for _, want := range []string{"Guidelines:", "rg --files", "Use read_file to read a single file", "Use edit_file for targeted changes", "Use write_file to create a file"} {
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
	if !strings.Contains(prompt, "Use mcp__* tools only when the user explicitly requests MCP") {
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
	if !strings.Contains(out, "offset is past the end of the file") || !strings.Contains(out, "total lines=4") {
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
	if !strings.Contains(out, "path is outside the working directory") {
		t.Fatalf("parent traversal output = %q", out)
	}
	out = registry.Execute(context.Background(), "read_file", map[string]string{"path": filepath.Join(outside, "secret.txt")})
	if !strings.Contains(out, "path is outside the working directory") {
		t.Fatalf("absolute traversal output = %q", out)
	}
	out = registry.Execute(context.Background(), "read_file", map[string]string{"path": "escape/secret.txt"})
	if strings.Contains(out, "outside-secret") || !strings.Contains(out, "Tool execution failed") {
		t.Fatalf("symlink read output = %q", out)
	}
	out = registry.Execute(context.Background(), "write_file", map[string]string{"path": "escape/new.txt", "content": "bad"})
	if !strings.Contains(out, "Tool execution failed") {
		t.Fatalf("symlink write output = %q", out)
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("symlink write escaped workspace: %v", err)
	}
	out = registry.Execute(context.Background(), "read_file", map[string]string{"path": "final-link"})
	if strings.Contains(out, "outside-secret") || !strings.Contains(out, "Tool execution failed") {
		t.Fatalf("final symlink read output = %q", out)
	}
	out = registry.Execute(context.Background(), "read_file", map[string]string{"path": "escape/"})
	if strings.Contains(out, "outside-secret") || !strings.Contains(out, "Tool execution failed") {
		t.Fatalf("trailing slash escape output = %q", out)
	}
	out = registry.Execute(context.Background(), "write_file", map[string]string{"path": ".git/config", "content": "bad"})
	if !strings.Contains(out, "must not modify .git directly") {
		t.Fatalf("git metadata output = %q", out)
	}
}

func TestFileToolsRejectGitMetadataCaseVariants(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	for _, path := range []string{".GIT/config", ".Git/hooks/pre-commit", "sub/.gIt/config"} {
		out := registry.Execute(context.Background(), "write_file", map[string]string{"path": path, "content": "bad"})
		if !strings.Contains(out, "must not modify .git directly") {
			t.Fatalf("case variant %q output = %q", path, out)
		}
		out = registry.Execute(context.Background(), "edit_file", map[string]string{"path": path, "old_text": "a", "new_text": "b"})
		if !strings.Contains(out, "must not modify .git directly") {
			t.Fatalf("case variant edit %q output = %q", path, out)
		}
	}
}

func TestFileToolsRejectSymlinkIntoGitMetadata(t *testing.T) {
	workspace := t.TempDir()
	hooks := filepath.Join(workspace, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(workspace, ".git"), filepath.Join(workspace, "innocent")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hooks, filepath.Join(workspace, "nested", "link")); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(workspace)
	for _, path := range []string{"innocent/hooks/pre-commit", "nested/link/pre-commit"} {
		out := registry.Execute(context.Background(), "write_file", map[string]string{"path": path, "content": "#!/bin/sh\necho pwned"})
		if !strings.Contains(out, "must not modify .git directly") {
			t.Fatalf("symlinked git write %q output = %q", path, out)
		}
	}
	if _, err := os.Stat(filepath.Join(hooks, "pre-commit")); !os.IsNotExist(err) {
		t.Fatalf("hook file must not exist: %v", err)
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
	if !strings.Contains(out, "read-only mode") {
		t.Fatalf("read-only output = %q", out)
	}
}

func TestSandboxPolicyHidesAndRejectsUnknownToolBeforeHITL(t *testing.T) {
	workspace := t.TempDir()
	manager, err := sandbox.New(context.Background(), sandbox.Options{Workspace: workspace, HomeDir: t.TempDir(), Mode: sandbox.ModeReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	var executed atomic.Int32
	hitl := &countingApprovalHandler{}
	registry := EmptyRegistry(workspace).WithSandbox(manager).WithHITL(hitl)
	registry.Register(Tool{
		Name:          "extension_write",
		Description:   "unclassified extension",
		Parameters:    mustSchema(t, `{"type":"object","properties":{}}`),
		PromptSnippet: "must stay hidden",
		Exec: func(context.Context, map[string]string) (string, error) {
			executed.Add(1)
			return "bad", nil
		},
	})
	if defs := registry.Definitions(); len(defs) != 0 {
		t.Fatalf("definitions = %+v", defs)
	}
	if prompt := registry.BuildPrompt(); strings.Contains(prompt, "extension_write") {
		t.Fatalf("hidden tool leaked into prompt: %s", prompt)
	}
	out := registry.Execute(context.Background(), "extension_write", nil)
	if !strings.Contains(out, "required=full-access") {
		t.Fatalf("policy output = %q", out)
	}
	if got := executed.Load(); got != 0 {
		t.Fatalf("executor calls = %d", got)
	}
	if got := hitl.requests.Load(); got != 0 {
		t.Fatalf("HITL requests = %d", got)
	}
}

func TestSandboxPolicyAllowsWorkspaceToolAndRevalidatesGeneration(t *testing.T) {
	workspace := t.TempDir()
	manager, err := sandbox.New(context.Background(), sandbox.Options{Workspace: workspace, HomeDir: t.TempDir(), Mode: sandbox.ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	var executed atomic.Int32
	hitl := &modeChangingApprovalHandler{manager: manager}
	registry := EmptyRegistry(workspace).WithSandbox(manager).WithHITL(hitl)
	registry.Register(Tool{
		Name:        "mcp_write",
		Description: "workspace writer",
		Parameters:  mustSchema(t, `{"type":"object","properties":{}}`),
		Policy:      Policy{Source: SourceMCP, MinimumMode: sandbox.ModeWorkspaceWrite},
		Exec: func(context.Context, map[string]string) (string, error) {
			executed.Add(1)
			return "bad", nil
		},
	})
	out := registry.Execute(context.Background(), "mcp_write", nil)
	if !strings.Contains(out, "read-only mode") {
		t.Fatalf("generation revalidation output = %q", out)
	}
	if got := executed.Load(); got != 0 {
		t.Fatalf("executor calls = %d", got)
	}
}

type countingApprovalHandler struct {
	requests atomic.Int32
}

func (*countingApprovalHandler) Enabled() bool     { return true }
func (*countingApprovalHandler) SetEnabled(bool)   {}
func (*countingApprovalHandler) ClearApprovedAll() {}
func (h *countingApprovalHandler) Request(context.Context, approval.Request) (approval.Result, error) {
	h.requests.Add(1)
	return approval.Approve(), nil
}

type modeChangingApprovalHandler struct {
	manager *sandbox.Manager
}

func (*modeChangingApprovalHandler) Enabled() bool     { return true }
func (*modeChangingApprovalHandler) SetEnabled(bool)   {}
func (*modeChangingApprovalHandler) ClearApprovedAll() {}
func (h *modeChangingApprovalHandler) Request(context.Context, approval.Request) (approval.Result, error) {
	_ = h.manager.SetMode(sandbox.ModeReadOnly)
	return approval.Approve(), nil
}

func TestParallelToolCallExecutor(t *testing.T) {
	registry := EmptyRegistry(t.TempDir())
	var running int32
	var maxSeen int32
	registry.Register(Tool{
		Name:        "sleepy",
		Description: "sleepy fake tool",
		Parameters:  mustSchema(t, `{"type":"object","properties":{"value":{"type":"string"}}}`),
		Policy:      Policy{ParallelSafe: true},
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
	results := executor.Execute(context.Background(), calls, ExecutionHooks{})
	if len(results) != 2 || results[0].Result != "a" || results[1].Result != "b" {
		t.Fatalf("results = %+v", results)
	}
	if atomic.LoadInt32(&maxSeen) < 2 {
		t.Fatalf("expected parallel execution, maxSeen=%d", maxSeen)
	}
}

func TestParallelSafeWorkerCountRespectsLimit(t *testing.T) {
	registry := EmptyRegistry(t.TempDir())
	started := make(chan string, 4)
	release := make(chan struct{})
	registry.Register(Tool{
		Name:       "limited",
		Parameters: mustSchema(t, `{"type":"object","properties":{"value":{"type":"string"}}}`),
		Policy:     Policy{ParallelSafe: true},
		Exec: func(_ context.Context, args map[string]string) (string, error) {
			started <- args["value"]
			<-release
			return args["value"], nil
		},
	})
	calls := make([]llm.ToolCall, 4)
	for index := range calls {
		calls[index] = llm.ToolCall{ID: fmt.Sprintf("%d", index), Function: llm.FunctionCall{Name: "limited", Arguments: fmt.Sprintf(`{"value":"%d"}`, index)}}
	}
	done := make(chan []ToolCallResult, 1)
	go func() {
		done <- (ParallelExecutor{Registry: registry, Config: runtime.ConcurrencyConfig{MaxParallelism: 2, BatchTimeout: time.Second}}).Execute(context.Background(), calls, ExecutionHooks{})
	}()
	<-started
	<-started
	select {
	case value := <-started:
		t.Fatalf("third tool started before a worker was released: %s", value)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	results := <-done
	for _, result := range results {
		if result.Status != ToolCallSuccess {
			t.Fatalf("result = %+v", result)
		}
	}
}

func TestParallelSafeCallsReturnInInputOrderAfterOutOfOrderCompletion(t *testing.T) {
	registry := EmptyRegistry(t.TempDir())
	started := make(chan string, 2)
	executed := make(chan string, 2)
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	registry.Register(Tool{
		Name:       "ordered",
		Parameters: mustSchema(t, `{"type":"object","properties":{"value":{"type":"string"}}}`),
		Policy:     Policy{ParallelSafe: true},
		Exec: func(_ context.Context, args map[string]string) (string, error) {
			started <- args["value"]
			if args["value"] == "a" {
				<-releaseA
			} else {
				<-releaseB
			}
			executed <- args["value"]
			return args["value"], nil
		},
	})
	calls := []llm.ToolCall{
		{ID: "1", Function: llm.FunctionCall{Name: "ordered", Arguments: `{"value":"a"}`}},
		{ID: "2", Function: llm.FunctionCall{Name: "ordered", Arguments: `{"value":"b"}`}},
	}
	done := make(chan []ToolCallResult, 1)
	go func() {
		done <- (ParallelExecutor{Registry: registry, Config: runtime.ConcurrencyConfig{MaxParallelism: 2, BatchTimeout: time.Second}}).Execute(context.Background(), calls, ExecutionHooks{})
	}()
	<-started
	<-started
	close(releaseB)
	if value := <-executed; value != "b" {
		t.Fatalf("first completed call = %q", value)
	}
	close(releaseA)
	results := <-done
	if results[0].Result != "a" || results[1].Result != "b" {
		t.Fatalf("results = %+v", results)
	}
}

func TestExclusiveToolCallsRunInInputOrder(t *testing.T) {
	registry := EmptyRegistry(t.TempDir())
	var mu sync.Mutex
	var order []string
	registry.Register(Tool{
		Name:       "exclusive",
		Parameters: mustSchema(t, `{"type":"object","properties":{"value":{"type":"string"}}}`),
		Exec: func(_ context.Context, args map[string]string) (string, error) {
			mu.Lock()
			order = append(order, args["value"])
			mu.Unlock()
			return args["value"], nil
		},
	})
	calls := []llm.ToolCall{
		{ID: "1", Function: llm.FunctionCall{Name: "exclusive", Arguments: `{"value":"a"}`}},
		{ID: "2", Function: llm.FunctionCall{Name: "exclusive", Arguments: `{"value":"b"}`}},
		{ID: "3", Function: llm.FunctionCall{Name: "exclusive", Arguments: `{"value":"c"}`}},
	}
	results := (ParallelExecutor{Registry: registry, Config: runtime.ConcurrencyConfig{MaxParallelism: 4, BatchTimeout: time.Second}}).Execute(context.Background(), calls, ExecutionHooks{})
	if got := strings.Join(order, ""); got != "abc" {
		t.Fatalf("exclusive execution order = %q", got)
	}
	for index, result := range results {
		if result.Result != string(rune('a'+index)) || result.Status != ToolCallSuccess {
			t.Fatalf("result[%d] = %+v", index, result)
		}
	}
}

func TestParallelismOnePreservesOrderForParallelSafeCalls(t *testing.T) {
	registry := EmptyRegistry(t.TempDir())
	var order []string
	registry.Register(Tool{
		Name:       "safe",
		Parameters: mustSchema(t, `{"type":"object","properties":{"value":{"type":"string"}}}`),
		Policy:     Policy{ParallelSafe: true},
		Exec: func(_ context.Context, args map[string]string) (string, error) {
			order = append(order, args["value"])
			return args["value"], nil
		},
	})
	calls := []llm.ToolCall{
		{ID: "1", Function: llm.FunctionCall{Name: "safe", Arguments: `{"value":"a"}`}},
		{ID: "2", Function: llm.FunctionCall{Name: "safe", Arguments: `{"value":"b"}`}},
		{ID: "3", Function: llm.FunctionCall{Name: "safe", Arguments: `{"value":"c"}`}},
	}
	results := (ParallelExecutor{Registry: registry, Config: runtime.ConcurrencyConfig{MaxParallelism: 1, BatchTimeout: time.Second}}).Execute(context.Background(), calls, ExecutionHooks{})
	if strings.Join(order, "") != "abc" {
		t.Fatalf("serial order = %v", order)
	}
	for _, result := range results {
		if result.Status != ToolCallSuccess {
			t.Fatalf("result = %+v", result)
		}
	}
}

func TestExclusiveToolIsBarrierBetweenParallelSafeSegments(t *testing.T) {
	registry := EmptyRegistry(t.TempDir())
	var safeRunning atomic.Int32
	var exclusiveRunning atomic.Int32
	var violation atomic.Bool
	registry.Register(Tool{
		Name:       "safe",
		Parameters: mustSchema(t, `{"type":"object","properties":{}}`),
		Policy:     Policy{ParallelSafe: true},
		Exec: func(context.Context, map[string]string) (string, error) {
			if exclusiveRunning.Load() != 0 {
				violation.Store(true)
			}
			safeRunning.Add(1)
			time.Sleep(10 * time.Millisecond)
			safeRunning.Add(-1)
			return "safe", nil
		},
	})
	registry.Register(Tool{
		Name:       "exclusive",
		Parameters: mustSchema(t, `{"type":"object","properties":{}}`),
		Exec: func(context.Context, map[string]string) (string, error) {
			exclusiveRunning.Add(1)
			if safeRunning.Load() != 0 {
				violation.Store(true)
			}
			time.Sleep(10 * time.Millisecond)
			exclusiveRunning.Add(-1)
			return "exclusive", nil
		},
	})
	calls := []llm.ToolCall{
		{ID: "1", Function: llm.FunctionCall{Name: "safe", Arguments: `{}`}},
		{ID: "2", Function: llm.FunctionCall{Name: "safe", Arguments: `{}`}},
		{ID: "3", Function: llm.FunctionCall{Name: "exclusive", Arguments: `{}`}},
		{ID: "4", Function: llm.FunctionCall{Name: "safe", Arguments: `{}`}},
		{ID: "5", Function: llm.FunctionCall{Name: "safe", Arguments: `{}`}},
	}
	results := (ParallelExecutor{Registry: registry, Config: runtime.ConcurrencyConfig{MaxParallelism: 2, BatchTimeout: time.Second}}).Execute(context.Background(), calls, ExecutionHooks{})
	if violation.Load() {
		t.Fatal("exclusive tool overlapped a parallel-safe segment")
	}
	for _, result := range results {
		if result.Status != ToolCallSuccess {
			t.Fatalf("result = %+v", result)
		}
	}
}

func TestBatchTimeoutReturnsWithoutWaitingForUncooperativeTool(t *testing.T) {
	registry := EmptyRegistry(t.TempDir())
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	registry.Register(Tool{
		Name:       "blocked",
		Parameters: mustSchema(t, `{"type":"object","properties":{}}`),
		Exec: func(context.Context, map[string]string) (string, error) {
			close(started)
			<-release
			return "late", nil
		},
	})
	executor := ParallelExecutor{Registry: registry, Config: runtime.ConcurrencyConfig{MaxParallelism: 1, BatchTimeout: 25 * time.Millisecond}}
	begin := time.Now()
	results := executor.Execute(context.Background(), []llm.ToolCall{{ID: "1", Function: llm.FunctionCall{Name: "blocked", Arguments: `{}`}}}, ExecutionHooks{})
	if elapsed := time.Since(begin); elapsed > 250*time.Millisecond {
		t.Fatalf("batch timeout returned too late: %s", elapsed)
	}
	select {
	case <-started:
	default:
		t.Fatal("tool was never started")
	}
	if results[0].Status != ToolCallTimeout {
		t.Fatalf("result = %+v", results[0])
	}
}

func TestBatchTimeoutFinalizesMultipleCallsAndDropsLateResults(t *testing.T) {
	registry := EmptyRegistry(t.TempDir())
	release := make(chan struct{})
	returned := make(chan struct{}, 2)
	var started atomic.Int32
	var completed atomic.Int32
	registry.Register(Tool{
		Name:       "blocked-safe",
		Parameters: mustSchema(t, `{"type":"object","properties":{}}`),
		Policy:     Policy{ParallelSafe: true},
		Exec: func(context.Context, map[string]string) (string, error) {
			started.Add(1)
			<-release
			returned <- struct{}{}
			return "late", nil
		},
	})
	calls := []llm.ToolCall{
		{ID: "1", Function: llm.FunctionCall{Name: "blocked-safe", Arguments: `{}`}},
		{ID: "2", Function: llm.FunctionCall{Name: "blocked-safe", Arguments: `{}`}},
		{ID: "3", Function: llm.FunctionCall{Name: "blocked-safe", Arguments: `{}`}},
	}
	results := (ParallelExecutor{Registry: registry, Config: runtime.ConcurrencyConfig{MaxParallelism: 2, BatchTimeout: 20 * time.Millisecond}}).Execute(
		context.Background(),
		calls,
		ExecutionHooks{OnCompleted: func(ToolCallResult) { completed.Add(1) }},
	)
	if started.Load() != 2 {
		t.Fatalf("started calls = %d, want 2", started.Load())
	}
	for _, result := range results {
		if result.Status != ToolCallTimeout {
			t.Fatalf("result = %+v", result)
		}
	}
	if completed.Load() != 3 {
		t.Fatalf("completed events = %d, want 3", completed.Load())
	}
	close(release)
	<-returned
	<-returned
	if completed.Load() != 3 {
		t.Fatalf("late results emitted extra completion events: %d", completed.Load())
	}
}

func TestBatchTimeoutDoesNotStartQueuedExclusiveCalls(t *testing.T) {
	registry := EmptyRegistry(t.TempDir())
	var secondStarted atomic.Bool
	registry.Register(Tool{
		Name:       "first",
		Parameters: mustSchema(t, `{"type":"object","properties":{}}`),
		Exec: func(ctx context.Context, _ map[string]string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	})
	registry.Register(Tool{
		Name:       "second",
		Parameters: mustSchema(t, `{"type":"object","properties":{}}`),
		Exec: func(context.Context, map[string]string) (string, error) {
			secondStarted.Store(true)
			return "bad", nil
		},
	})
	calls := []llm.ToolCall{
		{ID: "1", Function: llm.FunctionCall{Name: "first", Arguments: `{}`}},
		{ID: "2", Function: llm.FunctionCall{Name: "second", Arguments: `{}`}},
	}
	var eventMu sync.Mutex
	var startedIDs, completedIDs []string
	results := (ParallelExecutor{Registry: registry, Config: runtime.ConcurrencyConfig{MaxParallelism: 4, BatchTimeout: 20 * time.Millisecond}}).Execute(context.Background(), calls, ExecutionHooks{
		OnStarted: func(call llm.ToolCall) {
			eventMu.Lock()
			startedIDs = append(startedIDs, call.ID)
			eventMu.Unlock()
		},
		OnCompleted: func(result ToolCallResult) {
			eventMu.Lock()
			completedIDs = append(completedIDs, result.ToolCall.ID)
			eventMu.Unlock()
		},
	})
	if secondStarted.Load() {
		t.Fatal("queued exclusive tool started after the batch deadline")
	}
	if results[0].Status != ToolCallTimeout || results[1].Status != ToolCallTimeout {
		t.Fatalf("results = %+v", results)
	}
	if strings.Join(startedIDs, ",") != "1" || len(completedIDs) != 2 {
		t.Fatalf("started=%v completed=%v", startedIDs, completedIDs)
	}
}

func TestParentCancellationInterruptsRunningBatch(t *testing.T) {
	registry := EmptyRegistry(t.TempDir())
	started := make(chan struct{})
	registry.Register(Tool{
		Name:       "wait",
		Parameters: mustSchema(t, `{"type":"object","properties":{}}`),
		Exec: func(ctx context.Context, _ map[string]string) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan []ToolCallResult, 1)
	go func() {
		done <- (ParallelExecutor{Registry: registry, Config: runtime.ConcurrencyConfig{MaxParallelism: 1, BatchTimeout: time.Second}}).Execute(
			ctx,
			[]llm.ToolCall{{ID: "1", Function: llm.FunctionCall{Name: "wait", Arguments: `{}`}}},
			ExecutionHooks{},
		)
	}()
	<-started
	cancel()
	select {
	case results := <-done:
		if results[0].Status != ToolCallInterrupted {
			t.Fatalf("result = %+v", results[0])
		}
	case <-time.After(time.Second):
		t.Fatal("canceled batch did not return")
	}
}

func TestExecuteCommandTimeoutHasTimeoutStatus(t *testing.T) {
	registry := NewRegistry(t.TempDir()).WithConcurrency(runtime.ConcurrencyConfig{
		MaxParallelism: 1,
		BatchTimeout:   time.Second,
		CommandTimeout: 20 * time.Millisecond,
		MaxOutputChars: 1000,
	})
	result := (ParallelExecutor{Registry: registry, Config: runtime.ConcurrencyConfig{MaxParallelism: 1, BatchTimeout: time.Second}}).Execute(
		context.Background(),
		[]llm.ToolCall{{ID: "1", Function: llm.FunctionCall{Name: "execute_command", Arguments: `{"command":"sleep 1"}`}}},
		ExecutionHooks{},
	)[0]
	if result.Status != ToolCallTimeout || !strings.Contains(result.Result, "timed out") {
		t.Fatalf("result = %+v", result)
	}
}

func TestToolPanicBecomesFailedResult(t *testing.T) {
	registry := EmptyRegistry(t.TempDir())
	registry.Register(Tool{
		Name:       "panic",
		Parameters: mustSchema(t, `{"type":"object","properties":{}}`),
		Exec: func(context.Context, map[string]string) (string, error) {
			panic("boom")
		},
	})
	result := (ParallelExecutor{Registry: registry, Config: runtime.DefaultConcurrency()}).Execute(
		context.Background(),
		[]llm.ToolCall{{ID: "1", Function: llm.FunctionCall{Name: "panic", Arguments: `{}`}}},
		ExecutionHooks{},
	)[0]
	if result.Status != ToolCallFailed || !strings.Contains(result.Result, "panic: boom") {
		t.Fatalf("result = %+v", result)
	}
	outcome := registry.ExecuteResult(context.Background(), "panic", map[string]string{})
	if outcome.Status != ToolCallFailed || !strings.Contains(outcome.Output, "panic: boom") {
		t.Fatalf("registry outcome = %+v", outcome)
	}
}

type delayedApprovalHandler struct {
	delay  time.Duration
	result approval.Result
}

func (*delayedApprovalHandler) Enabled() bool     { return true }
func (*delayedApprovalHandler) SetEnabled(bool)   {}
func (*delayedApprovalHandler) ClearApprovedAll() {}
func (h *delayedApprovalHandler) Request(ctx context.Context, _ approval.Request) (approval.Result, error) {
	timer := time.NewTimer(h.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return approval.Result{}, ctx.Err()
	case <-timer.C:
		return h.result, nil
	}
}

func TestApprovalWaitDoesNotConsumeBatchTimeout(t *testing.T) {
	registry := EmptyRegistry(t.TempDir()).WithHITL(&delayedApprovalHandler{delay: 40 * time.Millisecond, result: approval.Approve()})
	registry.Register(Tool{
		Name:       "write_file",
		Parameters: mustSchema(t, `{"type":"object","properties":{}}`),
		Exec:       func(context.Context, map[string]string) (string, error) { return "done", nil },
	})
	result := (ParallelExecutor{Registry: registry, Config: runtime.ConcurrencyConfig{MaxParallelism: 1, BatchTimeout: 10 * time.Millisecond}}).Execute(
		context.Background(),
		[]llm.ToolCall{{ID: "1", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"x.txt"}`}}},
		ExecutionHooks{},
	)[0]
	if result.Status != ToolCallSuccess || result.Result != "done" {
		t.Fatalf("result = %+v", result)
	}
}

func TestStructuredToolCallStatuses(t *testing.T) {
	t.Run("unknown", func(t *testing.T) {
		result := RunToolCall(context.Background(), EmptyRegistry(t.TempDir()), llm.ToolCall{ID: "1", Function: llm.FunctionCall{Name: "missing", Arguments: `{}`}})
		if result.Status != ToolCallFailed {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("rejected", func(t *testing.T) {
		registry := EmptyRegistry(t.TempDir()).WithHITL(&delayedApprovalHandler{result: approval.Reject("no")})
		registry.Register(Tool{Name: "write_file", Parameters: mustSchema(t, `{"type":"object","properties":{}}`), Exec: func(context.Context, map[string]string) (string, error) { return "bad", nil }})
		result := RunToolCall(context.Background(), registry, llm.ToolCall{ID: "1", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"x.txt"}`}})
		if result.Status != ToolCallRejected {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("skipped", func(t *testing.T) {
		registry := EmptyRegistry(t.TempDir()).WithHITL(&delayedApprovalHandler{result: approval.Skip()})
		registry.Register(Tool{Name: "write_file", Parameters: mustSchema(t, `{"type":"object","properties":{}}`), Exec: func(context.Context, map[string]string) (string, error) { return "bad", nil }})
		result := RunToolCall(context.Background(), registry, llm.ToolCall{ID: "1", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"x.txt"}`}})
		if result.Status != ToolCallSkipped {
			t.Fatalf("result = %+v", result)
		}
	})
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
