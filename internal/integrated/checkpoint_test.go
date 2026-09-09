package integrated

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"bruce-go/internal/agent"
	"bruce-go/internal/checkpoint"
	"bruce-go/internal/event"
	"bruce-go/internal/llm"
	"bruce-go/internal/runtime"
	"bruce-go/internal/tool"
)

type checkpointClient struct {
	agent.FakeClient
	chat func(context.Context, []llm.Message) (llm.ChatResponse, error)
}

func (c *checkpointClient) Chat(ctx context.Context, messages []llm.Message, _ []llm.ToolDefinition, _ llm.StreamOptions) (llm.ChatResponse, error) {
	c.Calls++
	return c.chat(ctx, messages)
}

func newCheckpointRuntime(t *testing.T, workspace, home string, client llm.ChatClient) *Runtime {
	t.Helper()
	r, err := New(context.Background(), Options{Workspace: workspace, HomeDir: home, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, r)
	return r
}

func TestCheckpointCrashHelper(t *testing.T) {
	phase := os.Getenv("BRUCE_CHECKPOINT_CRASH_PHASE")
	if phase == "" {
		return
	}
	workspace, home := os.Getenv("BRUCE_CHECKPOINT_WORKSPACE"), os.Getenv("BRUCE_CHECKPOINT_HOME")
	client := &checkpointClient{}
	client.chat = func(_ context.Context, _ []llm.Message) (llm.ChatResponse, error) {
		if client.Calls == 1 {
			return llm.ChatResponse{ToolCalls: []llm.ToolCall{
				{ID: "write_a", Function: llm.FunctionCall{Name: "checkpoint_write", Arguments: `{"path":"a.txt"}`}},
				{ID: "write_b", Function: llm.FunctionCall{Name: "checkpoint_write", Arguments: `{"path":"b.txt"}`}},
			}}, nil
		}
		if phase == "model" {
			os.Exit(77)
		}
		return llm.ChatResponse{Content: "verified and finished"}, nil
	}
	r := newCheckpointRuntime(t, workspace, home, client)
	r.Tools.Register(tool.Tool{Name: "checkpoint_write", Parameters: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`), Exec: func(_ context.Context, args map[string]string) (string, error) {
		path := args["path"]
		err := os.WriteFile(filepath.Join(workspace, path), []byte("written once"), 0o644)
		return "wrote " + path, err
	}})
	r.Events.Subscribe(func(evt event.Event) {
		if e, ok := evt.(event.ToolCallCompleted); ok && phase == "tool" && e.Result.ToolCall.ID == "write_b" {
			os.Exit(77)
		}
		if e, ok := evt.(event.MessageCompleted); ok && phase == "final" && e.Message.Content == "verified and finished" {
			os.Exit(77)
		}
	})
	_, err := r.RunTask(context.Background(), "write two files then verify")
	t.Fatalf("helper did not crash: %v", err)
}

func TestCheckpointSurvivesProcessExitAndDoesNotReplayTools(t *testing.T) {
	for _, phase := range []string{"tool", "model", "final"} {
		t.Run(phase, func(t *testing.T) {
			workspace, home := t.TempDir(), t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestCheckpointCrashHelper$")
			cmd.Env = append(os.Environ(), "BRUCE_CHECKPOINT_CRASH_PHASE="+phase, "BRUCE_CHECKPOINT_WORKSPACE="+workspace, "BRUCE_CHECKPOINT_HOME="+home)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 77 {
				t.Fatalf("helper: %v\n%s", err, output)
			}
			client := &checkpointClient{chat: func(_ context.Context, messages []llm.Message) (llm.ChatResponse, error) {
				users, results := 0, map[string]int{}
				for _, msg := range messages {
					if msg.Role == llm.RoleUser && msg.Content == "write two files then verify" {
						users++
					}
					if msg.Role == llm.RoleTool {
						results[msg.ToolCallID]++
					}
				}
				if users != 1 || results["write_a"] != 1 || results["write_b"] != 1 {
					t.Fatalf("lost or duplicated transcript: %+v", messages)
				}
				for _, file := range []string{"a.txt", "b.txt"} {
					data, err := os.ReadFile(filepath.Join(workspace, file))
					if err != nil || string(data) != "written once" {
						t.Fatalf("lost workspace edits: %s %v", data, err)
					}
				}
				return llm.ChatResponse{Content: "verified and finished"}, nil
			}}
			r, err := New(context.Background(), Options{Workspace: workspace, HomeDir: home, Client: client, Resume: true})
			if err != nil {
				t.Fatal(err)
			}
			cleanupRuntime(t, r)
			// Any replay through the executor would be a regression.
			r.Tools.Register(tool.Tool{Name: "checkpoint_write", Exec: func(context.Context, map[string]string) (string, error) {
				t.Fatal("replayed completed tool")
				return "", nil
			}})
			out, err := r.ResumeTask(context.Background(), false)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "tool" {
				if client.Calls != 0 || !strings.Contains(out, "--accept-changes") {
					t.Fatalf("unacknowledged changes executed: %s", out)
				}
				out, err = r.ResumeTask(context.Background(), true)
			}
			if err != nil || out != "verified and finished" {
				t.Fatalf("resume: %q %v", out, err)
			}
			expected := 1
			if phase == "final" {
				expected = 0
			}
			if client.Calls != expected {
				t.Fatalf("model calls = %d, want %d", client.Calls, expected)
			}
			if r.Session.Context(r.Mode).Task.Status != checkpoint.Completed {
				t.Fatal("task was not completed")
			}
			if _, err := r.ResumeTask(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			if client.Calls != expected {
				t.Fatal("completed task ran again")
			}
		})
	}
}

func TestCheckpointNetworkFailureCancellationAndLimitAreResumable(t *testing.T) {
	for _, stop := range []string{"network", "cancel", "limit"} {
		t.Run(stop, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := &checkpointClient{chat: func(context.Context, []llm.Message) (llm.ChatResponse, error) {
				if stop == "cancel" {
					cancel()
					return llm.ChatResponse{}, context.Canceled
				}
				return llm.ChatResponse{}, errors.New("provider unavailable")
			}}
			r := newCheckpointRuntime(t, t.TempDir(), t.TempDir(), client)
			if stop == "limit" {
				r.react.MaxIterations = 0
			}
			if _, err := r.RunTask(ctx, "finish task"); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"network": checkpoint.Failed, "cancel": checkpoint.Interrupted, "limit": checkpoint.Limited}[stop]
			if task := r.Session.Context(r.Mode).Task; task.Status != want || !task.Resumable() {
				t.Fatalf("task = %+v", task)
			}
			client.chat = func(context.Context, []llm.Message) (llm.ChatResponse, error) {
				return llm.ChatResponse{Content: "done"}, nil
			}
			if out, err := r.ResumeTask(context.Background(), false); err != nil || out != "done" {
				t.Fatalf("resume: %s %v", out, err)
			}
		})
	}
}

func TestCheckpointWorkspaceEditsRequireAcknowledgement(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "user.txt")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	client := &checkpointClient{chat: func(context.Context, []llm.Message) (llm.ChatResponse, error) {
		return llm.ChatResponse{}, errors.New("offline")
	}}
	r := newCheckpointRuntime(t, workspace, t.TempDir(), client)
	_, _ = r.RunTask(context.Background(), "task")
	if err := os.WriteFile(path, []byte("user changes"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := client.Calls
	res := r.Handle(context.Background(), "/resume --continue")
	if res.Err != nil || client.Calls != before || !strings.Contains(res.Output, "modified: user.txt") {
		t.Fatalf("drift not gated: %+v", res)
	}
	client.chat = func(_ context.Context, messages []llm.Message) (llm.ChatResponse, error) {
		found := false
		for _, msg := range messages {
			if strings.Contains(msg.Content, "acknowledged these workspace changes") {
				found = true
			}
		}
		if !found {
			t.Fatal("model did not receive workspace reconciliation context")
		}
		return llm.ChatResponse{Content: "done"}, nil
	}
	res = r.Handle(context.Background(), "/resume --continue --accept-changes")
	if res.Err != nil || res.Output != "done" {
		t.Fatalf("accepted resume failed: %+v", res)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "user changes" {
		t.Fatal("user changes overwritten")
	}
}

func TestCheckpointResumeRejectsDifferentWorkspace(t *testing.T) {
	r := newCheckpointRuntime(t, t.TempDir(), t.TempDir(), &agent.FakeClient{})
	other := newCheckpointRuntime(t, t.TempDir(), r.HomeDir, &agent.FakeClient{})
	original := r.Session.File
	res := r.Handle(context.Background(), "/resume "+other.Session.File)
	if res.Err == nil || r.Session.File != original {
		t.Fatalf("foreign session replaced live state: %+v", res)
	}
}

func TestCheckpointPlanFailureIsNotPresentedForApproval(t *testing.T) {
	r := newCheckpointRuntime(t, t.TempDir(), t.TempDir(), &agent.FakeClient{Err: fmt.Errorf("offline")})
	if err := r.setMode(runtime.ModePlan); err != nil {
		t.Fatal(err)
	}
	_, _ = r.RunTask(context.Background(), "prepare a plan")
	state := r.Session.Context(r.Mode)
	if !state.ActivePlan.Empty() || state.Task.Status != checkpoint.Failed {
		t.Fatalf("failure became a plan: %+v", state)
	}
}
