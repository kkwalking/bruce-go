package plan

import (
	"context"
	"strings"
	"testing"

	"bruce-go/internal/runtime"
	"bruce-go/internal/tool"
)

func TestParserBuildsAndValidatesDAG(t *testing.T) {
	raw := `{
  "goal": "demo",
  "summary": "two steps",
  "tasks": [
    {"id":"task_read","description":"read","type":"FILE_READ","path":"a.txt"},
    {"id":"task_verify","description":"verify","type":"VERIFICATION","command":"printf ok","dependencies":["task_read"]}
  ]
}`
	plan, err := Parser{}.Parse(raw, "fallback")
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := plan.TopologicalOrder()
	if err != nil {
		t.Fatal(err)
	}
	if ordered[0].ID != "task_read" || ordered[1].ID != "task_verify" {
		t.Fatalf("order = %v, %v", ordered[0].ID, ordered[1].ID)
	}
}

func TestParserRejectsMissingDependency(t *testing.T) {
	raw := `{"tasks":[{"id":"task_a","description":"a","type":"ANALYSIS","dependencies":["task_missing"]}]}`
	_, err := Parser{}.Parse(raw, "fallback")
	if err == nil || !strings.Contains(err.Error(), "dependency does not exist") {
		t.Fatalf("expected missing dependency error, got %v", err)
	}
}

func TestExecutorRunsPlanTasks(t *testing.T) {
	dir := t.TempDir()
	registry := tool.NewRegistry(dir)
	p := NewExecutionPlan("write then read")
	if err := p.AddTask(&Task{ID: "task_write", Description: "write", Type: TaskFileWrite, Path: "out.txt", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := p.AddTask(&Task{ID: "task_read", Description: "read", Type: TaskFileRead, Path: "out.txt", Dependencies: []string{"task_write"}}); err != nil {
		t.Fatal(err)
	}
	report := Executor{Tools: registry, Config: runtime.DefaultConcurrency()}.Execute(context.Background(), p)
	if !report.Success() {
		t.Fatalf("report failed: %+v", report.Plan.Tasks)
	}
	if !strings.Contains(report.Plan.Tasks["task_read"].Result, "hello") {
		t.Fatalf("read result = %q", report.Plan.Tasks["task_read"].Result)
	}
}
