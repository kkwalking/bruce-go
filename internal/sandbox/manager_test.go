package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseMode(t *testing.T) {
	for _, raw := range []string{"read-only", "workspace-write", "full-access"} {
		if mode, err := ParseMode(raw); err != nil || string(mode) != raw {
			t.Fatalf("ParseMode(%q) = %q, %v", raw, mode, err)
		}
	}
	if _, err := ParseMode("unsafe"); err == nil {
		t.Fatal("invalid mode should fail")
	}
	if _, err := ParseMode("danger-full-access"); err == nil {
		t.Fatal("legacy danger-full-access mode should fail")
	}
}

func TestManagerDefaultsToFullAccess(t *testing.T) {
	manager, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	status := manager.Status()
	if status.Mode != ModeFullAccess || !status.NetworkAccess {
		t.Fatalf("default status = %+v", status)
	}
}

func TestManagerSafeEnvironment(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("BRUCE_TEST_ALLOWED", "visible")
	manager, err := New(context.Background(), Options{
		Workspace:  t.TempDir(),
		HomeDir:    t.TempDir(),
		Mode:       ModeWorkspaceWrite,
		AllowedEnv: []string{"BRUCE_TEST_ALLOWED"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	env := strings.Join(manager.safeEnvironment(false), "\n")
	if strings.Contains(env, "AWS_SECRET_ACCESS_KEY") || strings.Contains(env, "secret") {
		t.Fatalf("safe environment leaked secret:\n%s", env)
	}
	if !strings.Contains(env, "BRUCE_TEST_ALLOWED=visible") {
		t.Fatalf("explicit environment missing:\n%s", env)
	}
	if !strings.Contains(env, "BRUCE_SANDBOX=") || !strings.Contains(env, "BRUCE_SANDBOX_NETWORK_DISABLED=1") {
		t.Fatalf("sandbox markers missing:\n%s", env)
	}
}

func TestValidateWritableWorkspace(t *testing.T) {
	home := t.TempDir()
	if err := validateWritableWorkspace(home, home); err == nil {
		t.Fatal("HOME workspace should be rejected")
	}
	if err := validateWritableWorkspace(filepath.Dir(home), home); err == nil {
		t.Fatal("HOME ancestor workspace should be rejected")
	}
	if err := validateWritableWorkspace(t.TempDir(), home); err != nil {
		t.Fatalf("separate workspace should be accepted: %v", err)
	}
}

func TestManagerFileWritePolicy(t *testing.T) {
	manager, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Mode: ModeReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.CanWriteFile("a.txt"); err == nil {
		t.Fatal("read-only mode should reject writes")
	}
	if err := manager.SetMode(ModeFullAccess); err != nil {
		t.Fatal(err)
	}
	if err := manager.CanWriteFile(filepath.Join(".git", "config")); err == nil {
		t.Fatal("file tools should never write .git")
	}
	if _, err := os.Stat(manager.tempRoot); err != nil {
		t.Fatal(err)
	}
}

func TestFullAccessForcesEffectiveNetworkAccess(t *testing.T) {
	manager, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Mode: ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if manager.Status().NetworkAccess {
		t.Fatal("safe mode network should start disabled")
	}
	if err := manager.SetMode(ModeFullAccess); err != nil {
		t.Fatal(err)
	}
	if !manager.Status().NetworkAccess {
		t.Fatal("full-access should force effective network access")
	}
	if err := manager.SetNetworkAccess(false); !errors.Is(err, ErrPolicy) {
		t.Fatalf("disabling network in full-access should fail: %v", err)
	}
	if err := manager.SetMode(ModeWorkspaceWrite); err != nil {
		t.Fatal(err)
	}
	if manager.Status().NetworkAccess {
		t.Fatal("safe mode should restore the configured network setting")
	}
}

func TestManagerFailsClosedButAllowsExplicitDangerMode(t *testing.T) {
	manager, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Mode: ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	manager.capabilities = Capabilities{Backend: "test", Reason: "probe failed"}
	if err := manager.Preflight(nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("preflight error = %v", err)
	}
	if err := manager.SetMode(ModeFullAccess); err != nil {
		t.Fatal(err)
	}
	if err := manager.Preflight(nil); err != nil {
		t.Fatalf("full-access mode should bypass unavailable backend: %v", err)
	}
}

func TestManagerRejectsUntrustedGitMetadata(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".git"), []byte("gitdir: ../not-a-repository\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager, err := New(context.Background(), Options{Workspace: workspace, HomeDir: t.TempDir(), Mode: ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.Preflight(nil); !errors.Is(err, ErrPolicy) {
		t.Fatalf("untrusted git metadata should fail closed: %v", err)
	}
	if err := manager.SetMode(ModeFullAccess); err != nil {
		t.Fatalf("explicit full-access mode should remain available: %v", err)
	}
}

type recordingRunner struct {
	roots   chan string
	release chan struct{}
}

func (r recordingRunner) Probe(context.Context) Capabilities {
	return Capabilities{Backend: "recording", Available: true}
}

func (r recordingRunner) Run(_ context.Context, spec CommandSpec, policy Policy) (RunResult, error) {
	r.roots <- policy.TempRoot + "\n" + strings.Join(spec.Environment, "\n")
	<-r.release
	return RunResult{}, nil
}

func TestParallelCommandsUseIsolatedTemporaryRoots(t *testing.T) {
	manager, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Mode: ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	recorder := recordingRunner{roots: make(chan string, 2), release: make(chan struct{})}
	manager.runner = recorder
	manager.capabilities = Capabilities{Backend: "recording", Available: true}

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, runErr := manager.Run(context.Background(), "true", time.Second, 1000, nil); runErr != nil {
				t.Errorf("Run: %v", runErr)
			}
		}()
	}
	first := <-recorder.roots
	second := <-recorder.roots
	close(recorder.release)
	wg.Wait()
	firstRoot := strings.SplitN(first, "\n", 2)[0]
	secondRoot := strings.SplitN(second, "\n", 2)[0]
	if firstRoot == secondRoot {
		t.Fatalf("parallel commands shared temp root: %s", firstRoot)
	}
	for _, value := range []struct {
		root string
		env  string
	}{{firstRoot, first}, {secondRoot, second}} {
		if !strings.Contains(value.env, "TMPDIR="+filepath.Join(value.root, "tmp")) {
			t.Fatalf("environment does not use command root:\n%s", value.env)
		}
		if _, statErr := os.Stat(value.root); !os.IsNotExist(statErr) {
			t.Fatalf("command temp root was not cleaned: %s: %v", value.root, statErr)
		}
	}
}
