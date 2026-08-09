package sandbox

import (
	"bufio"
	"context"
	"errors"
	"io"
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

func TestManagerValidatesWorkspaceBeforeDynamicWorkspaceWrite(t *testing.T) {
	home := t.TempDir()
	manager, err := New(context.Background(), Options{
		Workspace: home,
		HomeDir:   home,
		Mode:      ModeFullAccess,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.SetMode(ModeWorkspaceWrite); err == nil || !strings.Contains(err.Error(), "overly broad") {
		t.Fatalf("dynamic workspace-write should reject HOME workspace: %v", err)
	}
	if manager.Mode() != ModeFullAccess {
		t.Fatalf("failed mode transition changed mode to %s", manager.Mode())
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
	manager.SetNetworkAccess(true)
	manager.SetNetworkAccess(false)
	status := manager.Status()
	if !status.NetworkAccess || status.ConfiguredNetworkAccess {
		t.Fatalf("full-access should keep effective network on while storing configured=off: %+v", status)
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

func (r recordingRunner) Name() string { return "recording" }

func (r recordingRunner) Probe(context.Context) Capabilities {
	return Capabilities{Backend: "recording", Available: true}
}

func (r recordingRunner) Run(_ context.Context, spec CommandSpec, policy Policy) (RunResult, error) {
	r.roots <- policy.TempRoot + "\n" + strings.Join(spec.Environment, "\n")
	<-r.release
	return RunResult{}, nil
}

func (recordingRunner) PrepareProcess(spec ProcessSpec, _ Policy) (PreparedProcess, error) {
	return PreparedProcess{Program: spec.Program, Args: spec.Args, Directory: spec.Directory, Environment: spec.Environment}, nil
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

func TestManagerStartsLongRunningArgvProcess(t *testing.T) {
	manager, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Mode: ModeFullAccess})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	process, err := manager.StartProcess(context.Background(), ProcessSpec{
		Program:     "/bin/sh",
		Args:        []string{"-c", `IFS= read -r line; printf '%s:%s\n' "$line" "$MCP_TOKEN"`},
		Environment: []string{"MCP_TOKEN=explicit"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if process.PID() <= 0 {
		t.Fatalf("PID = %d", process.PID())
	}
	if _, err := io.WriteString(process.Stdin(), "hello\n"); err != nil {
		t.Fatal(err)
	}
	_ = process.Stdin().Close()
	line, err := bufio.NewReader(process.Stdout()).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "hello:explicit\n" {
		t.Fatalf("output = %q", line)
	}
	if err := process.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRestrictedLongRunningProcessUsesSafeEnvironmentAndCleansTemp(t *testing.T) {
	t.Setenv("BRUCE_TEST_HIDDEN", "secret")
	manager, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Mode: ModeReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	manager.runner = recordingRunner{}
	manager.capabilities = Capabilities{Backend: "recording", Available: true}
	manager.probed = true
	process, err := manager.StartProcess(context.Background(), ProcessSpec{
		Program:     "/usr/bin/env",
		Environment: []string{"MCP_EXPLICIT=ok"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(process.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Wait(); err != nil {
		t.Fatal(err)
	}
	env := string(data)
	if strings.Contains(env, "BRUCE_TEST_HIDDEN=secret") {
		t.Fatalf("hidden host environment leaked:\n%s", env)
	}
	if !strings.Contains(env, "MCP_EXPLICIT=ok") {
		t.Fatalf("explicit MCP environment missing:\n%s", env)
	}
	var tempRoot string
	for _, line := range strings.Split(env, "\n") {
		if strings.HasPrefix(line, "TMPDIR=") {
			tempRoot = filepath.Dir(strings.TrimPrefix(line, "TMPDIR="))
		}
	}
	if tempRoot == "" {
		t.Fatalf("TMPDIR missing:\n%s", env)
	}
	if _, err := os.Stat(tempRoot); !os.IsNotExist(err) {
		t.Fatalf("process temp root was not cleaned: %s: %v", tempRoot, err)
	}
}

func TestRestrictedLongRunningProcessFailsClosedWhenBackendUnavailable(t *testing.T) {
	manager, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Mode: ModeFullAccess})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	manager.runner = unavailableTestRunner{}
	manager.capabilities = Capabilities{Backend: "unavailable-test", Reason: "disabled"}
	manager.probed = true
	manager.mode = ModeReadOnly
	if _, err := manager.StartProcess(context.Background(), ProcessSpec{Program: "/usr/bin/true"}, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("StartProcess error = %v", err)
	}
}

type unavailableTestRunner struct{}

func (unavailableTestRunner) Name() string { return "unavailable-test" }
func (unavailableTestRunner) Probe(context.Context) Capabilities {
	return Capabilities{Backend: "unavailable-test", Reason: "disabled"}
}
func (unavailableTestRunner) Run(context.Context, CommandSpec, Policy) (RunResult, error) {
	return RunResult{}, ErrUnavailable
}
func (unavailableTestRunner) PrepareProcess(ProcessSpec, Policy) (PreparedProcess, error) {
	return PreparedProcess{}, ErrUnavailable
}

type countingRunner struct {
	probes *int
}

func (r countingRunner) Name() string { return "counting" }

func (r countingRunner) Probe(context.Context) Capabilities {
	*r.probes++
	return Capabilities{Backend: "counting", Available: true}
}

func (r countingRunner) Run(context.Context, CommandSpec, Policy) (RunResult, error) {
	return RunResult{}, nil
}

func (countingRunner) PrepareProcess(spec ProcessSpec, _ Policy) (PreparedProcess, error) {
	return PreparedProcess{Program: spec.Program, Args: spec.Args, Directory: spec.Directory, Environment: spec.Environment}, nil
}

func TestProbeIsLazyInFullAccessMode(t *testing.T) {
	manager, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Mode: ModeFullAccess})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	probes := 0
	manager.runner = countingRunner{probes: &probes}

	status := manager.Status()
	if probes != 0 {
		t.Fatalf("Status should not trigger probe, probes=%d", probes)
	}
	if status.Capabilities.Backend != "counting" || status.Capabilities.Available {
		t.Fatalf("unprobed status should report runner name and unavailable: %+v", status.Capabilities)
	}
	if err := manager.Preflight(nil); err != nil {
		t.Fatalf("full-access preflight should not need probe: %v", err)
	}
	if probes != 0 {
		t.Fatalf("full-access preflight should not trigger probe, probes=%d", probes)
	}
	if err := manager.SetMode(ModeReadOnly); err != nil {
		t.Fatal(err)
	}
	if probes != 1 {
		t.Fatalf("switching to restricted mode should probe once, probes=%d", probes)
	}
	if err := manager.SetMode(ModeWorkspaceWrite); err != nil {
		t.Fatal(err)
	}
	if err := manager.Preflight(nil); err != nil {
		t.Fatal(err)
	}
	if probes != 1 {
		t.Fatalf("probe should run at most once, probes=%d", probes)
	}
}

func TestIsGitMetadataPath(t *testing.T) {
	for _, path := range []string{".git", ".GIT", ".Git/config", filepath.Join(".git", "hooks", "pre-commit"), filepath.Join("vendor", "repo", ".GiT", "config")} {
		if !IsGitMetadataPath(path) {
			t.Fatalf("IsGitMetadataPath(%q) should be true", path)
		}
	}
	for _, path := range []string{".", "src/main.go", ".gitignore", ".github/workflows/ci.yml", "git/config"} {
		if IsGitMetadataPath(path) {
			t.Fatalf("IsGitMetadataPath(%q) should be false", path)
		}
	}
}
