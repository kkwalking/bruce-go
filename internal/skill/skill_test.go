package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bruce-go/internal/tool"
)

func TestLoaderProjectOverridesUserAndToolsReadResources(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	writeSkill(t, filepath.Join(home, ".bruce", "skills", "review"), "review", "user desc", "user instructions")
	projectDir := filepath.Join(workspace, ".bruce", "skills", "review")
	writeSkill(t, projectDir, "review", "project desc", "project instructions")
	if err := os.WriteFile(filepath.Join(projectDir, "notes.md"), []byte("resource text"), 0o644); err != nil {
		t.Fatal(err)
	}

	catalog := NewCatalog(home, workspace)
	skills := catalog.Skills()
	if len(skills) != 1 || skills[0].Source != SourceProject || skills[0].Description != "project desc" {
		t.Fatalf("skills = %+v", skills)
	}
	if len(catalog.Overrides()) != 1 {
		t.Fatalf("overrides = %+v", catalog.Overrides())
	}

	registry := tool.EmptyRegistry(workspace)
	RegisterTools(registry, catalog)
	out := registry.Execute(context.Background(), "load_skill", map[string]string{"name": "review"})
	if !strings.Contains(out, "project instructions") {
		t.Fatalf("load_skill output = %q", out)
	}
	out = registry.Execute(context.Background(), "read_skill_resource", map[string]string{"skill": "review", "path": "notes.md"})
	if out != "resource text" {
		t.Fatalf("resource output = %q", out)
	}
}

func TestParseInvocation(t *testing.T) {
	inv, err := ParseInvocation("$review $security 审查代码")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(inv.Names, ",") != "review,security" || inv.Task != "审查代码" {
		t.Fatalf("invocation = %+v", inv)
	}
	if _, err := ParseInvocation("$review"); err == nil {
		t.Fatal("expected missing task error")
	}
}

func writeSkill(t *testing.T, dir, name, description, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
