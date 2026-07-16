//go:build linux

package sandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildBubblewrapArgs(t *testing.T) {
	temp := t.TempDir()
	workspace := t.TempDir()
	protected := filepath.Join(workspace, ".git", "config")
	if err := os.MkdirAll(filepath.Dir(protected), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(protected, []byte("config"), 0o644); err != nil {
		t.Fatal(err)
	}
	args, err := buildBubblewrapArgs(CommandSpec{Command: "pwd", Directory: workspace, Environment: []string{"PATH=/usr/bin:/bin"}}, Policy{
		Mode: ModeWorkspaceWrite, WorkspaceRoot: workspace, TempRoot: temp,
		Git: GitLayout{WriteRoots: []string{filepath.Dir(protected)}, ProtectedPaths: []string{protected}},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--unshare-net", "--ro-bind / /", "--bind " + workspace + " " + workspace, "--ro-bind " + protected + " " + protected, "--clearenv", "/bin/bash --noprofile --norc -c pwd"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q:\n%s", want, joined)
		}
	}
	if strings.Index(joined, "--bind "+workspace) > strings.Index(joined, "--ro-bind "+protected) {
		t.Fatalf("protected overlay must follow writable root:\n%s", joined)
	}
}

func TestValidateSystemBubblewrapRejectsWritablePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(path, []byte("not trusted"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := validateSystemBubblewrap(path); err == nil {
		t.Fatal("user-writable bwrap path should be rejected")
	}
}

func TestBubblewrapWorkspaceSensitiveAndNetwork(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	secretDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(secretDir, "id_test")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := New(context.Background(), Options{Workspace: workspace, HomeDir: home, Mode: ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if !manager.Status().Capabilities.Available {
		if os.Getenv("BRUCE_REQUIRE_SANDBOX_TESTS") == "1" {
			t.Fatalf("required bubblewrap unavailable: %+v", manager.Status())
		}
		t.Skipf("bubblewrap unavailable: %+v", manager.Status())
	}
	result, err := manager.Run(context.Background(), "printf inside > inside.txt", 10*time.Second, 4000, nil)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("workspace write = %+v, %v", result, err)
	}
	result, err = manager.Run(context.Background(), "cat "+linuxShellQuote(secret), 10*time.Second, 4000, nil)
	if err != nil || result.ExitCode == 0 || strings.Contains(result.Output, "secret") {
		t.Fatalf("sensitive read = %+v, %v", result, err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("network-ok")) }))
	defer server.Close()
	result, err = manager.Run(context.Background(), "curl -fsS "+linuxShellQuote(server.URL), 10*time.Second, 4000, nil)
	if err != nil || result.ExitCode == 0 {
		t.Fatalf("network should be denied: %+v, %v", result, err)
	}
	manager.SetNetworkAccess(true)
	result, err = manager.Run(context.Background(), "curl -fsS "+linuxShellQuote(server.URL), 10*time.Second, 4000, nil)
	if err != nil || result.ExitCode != 0 || !strings.Contains(result.Output, "network-ok") {
		t.Fatalf("network enabled = %+v, %v", result, err)
	}
}

func linuxShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
