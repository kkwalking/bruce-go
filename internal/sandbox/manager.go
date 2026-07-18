package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Options struct {
	Workspace     string
	HomeDir       string
	Mode          Mode
	NetworkAccess bool
	AllowedEnv    []string
}

type Manager struct {
	mu             sync.RWMutex
	workspace      string
	home           string
	tempRoot       string
	mode           Mode
	networkAccess  bool
	allowedEnv     []string
	runner         Runner
	capabilities   Capabilities
	probed         bool
	probeOnce      sync.Once
	sensitivePaths []string
	socketPaths    []string
	git            GitLayout
	policyErr      error
	generation     uint64
	closeOnce      sync.Once
}

func New(ctx context.Context, opts Options) (*Manager, error) {
	mode := opts.Mode
	if mode == "" {
		mode = ModeFullAccess
	}
	if _, err := ParseMode(string(mode)); err != nil {
		return nil, err
	}
	workspace, err := canonicalAbsolute(opts.Workspace)
	if err != nil {
		return nil, fmt.Errorf("解析 workspace: %w", err)
	}
	home, err := canonicalAbsolute(opts.HomeDir)
	if err != nil {
		return nil, fmt.Errorf("解析 HOME: %w", err)
	}
	tempRoot, err := os.MkdirTemp("", "bruce-sandbox-")
	if err != nil {
		return nil, fmt.Errorf("创建 sandbox 临时目录: %w", err)
	}
	for _, name := range []string{"tmp", "cache", "run"} {
		if err := os.MkdirAll(filepath.Join(tempRoot, name), 0o700); err != nil {
			_ = os.RemoveAll(tempRoot)
			return nil, fmt.Errorf("初始化 sandbox 临时目录: %w", err)
		}
	}

	m := &Manager{
		workspace:     workspace,
		home:          home,
		tempRoot:      tempRoot,
		mode:          mode,
		networkAccess: opts.NetworkAccess,
		allowedEnv:    append([]string(nil), opts.AllowedEnv...),
		generation:    1,
	}
	m.sensitivePaths, m.socketPaths = protectedHostPaths(home)
	var gitErr error
	m.git, gitErr = discoverGitLayout(workspace)
	if gitErr != nil {
		m.policyErr = fmt.Errorf("Git 元数据布局不可信: %w", gitErr)
	}
	if mode == ModeWorkspaceWrite {
		if err := validateWritableWorkspace(workspace, home); err != nil {
			m.policyErr = err
		}
		for _, sensitive := range m.sensitivePaths {
			if pathContains(sensitive, workspace) {
				m.policyErr = fmt.Errorf("workspace 位于敏感目录内: %s", sensitive)
				break
			}
		}
	}
	m.runner = newPlatformRunner(workspace)
	if mode != ModeFullAccess {
		m.ensureProbed(ctx)
	}
	return m, nil
}

func (m *Manager) ensureProbed(ctx context.Context) {
	m.probeOnce.Do(func() {
		capabilities := m.runner.Probe(ctx)
		m.mu.Lock()
		m.capabilities = capabilities
		m.probed = true
		m.mu.Unlock()
	})
}

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	var err error
	m.closeOnce.Do(func() { err = os.RemoveAll(m.tempRoot) })
	return err
}

func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	capabilities := m.capabilities
	if !m.probed {
		capabilities = Capabilities{Backend: m.runner.Name(), Reason: "backend 尚未探测，首次进入受限模式时探测"}
	}
	if m.policyErr != nil && m.mode != ModeFullAccess {
		capabilities.Available = false
		capabilities.Reason = m.policyErr.Error()
	}
	return Status{
		Mode:                    m.mode,
		NetworkAccess:           effectiveNetworkAccess(m.mode, m.networkAccess),
		ConfiguredNetworkAccess: m.networkAccess,
		Capabilities:            capabilities,
		Generation:              m.generation,
	}
}

func (m *Manager) Mode() Mode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.mode
}

func (m *Manager) SetMode(mode Mode) error {
	if err := m.ValidateMode(mode); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mode != mode {
		m.mode = mode
		m.generation++
	}
	return nil
}

func (m *Manager) ValidateMode(mode Mode) error {
	if _, err := ParseMode(string(mode)); err != nil {
		return err
	}
	if mode == ModeWorkspaceWrite {
		if err := validateWritableWorkspace(m.workspace, m.home); err != nil {
			return err
		}
		for _, sensitive := range m.sensitivePaths {
			if pathContains(sensitive, m.workspace) {
				return fmt.Errorf("workspace 位于敏感目录内: %s", sensitive)
			}
		}
	}
	if mode != ModeFullAccess {
		m.ensureProbed(context.Background())
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if mode != ModeFullAccess {
		if m.policyErr != nil {
			return m.policyErr
		}
		if !m.capabilities.Available {
			return fmt.Errorf("%w: %s", ErrUnavailable, m.capabilities.Reason)
		}
	}
	return nil
}

func (m *Manager) SetNetworkAccess(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.networkAccess != enabled {
		m.networkAccess = enabled
		m.generation++
	}
}

func (m *Manager) CanWriteFile(relativePath string) error {
	if IsGitMetadataPath(relativePath) {
		return fmt.Errorf("%w: 文件工具禁止直接修改 .git", ErrPolicy)
	}
	if m.Mode() == ModeReadOnly {
		return fmt.Errorf("%w: read-only 模式禁止修改 workspace", ErrPolicy)
	}
	return nil
}

func (m *Manager) Preflight(modeOverride *Mode) error {
	mode, _, capabilities, policyErr := m.snapshot(modeOverride)
	return preflightSnapshot(mode, capabilities, policyErr)
}

func preflightSnapshot(mode Mode, capabilities Capabilities, policyErr error) error {
	if mode == ModeFullAccess {
		return nil
	}
	if policyErr != nil {
		return fmt.Errorf("%w: %v", ErrPolicy, policyErr)
	}
	if !capabilities.Available {
		return fmt.Errorf("%w: backend=%s: %s", ErrUnavailable, capabilities.Backend, capabilities.Reason)
	}
	return nil
}

func (m *Manager) Run(ctx context.Context, command string, timeout time.Duration, maxOutputChars int, modeOverride *Mode) (RunResult, error) {
	mode, network, capabilities, policyErr := m.snapshot(modeOverride)
	if err := preflightSnapshot(mode, capabilities, policyErr); err != nil {
		return RunResult{}, err
	}
	commandTempRoot := m.tempRoot
	if mode != ModeFullAccess {
		var err error
		commandTempRoot, err = m.newCommandTempRoot()
		if err != nil {
			return RunResult{}, fmt.Errorf("初始化命令隔离目录: %w", err)
		}
		defer os.RemoveAll(commandTempRoot)
	}
	policy := Policy{
		Mode:           mode,
		NetworkAccess:  network,
		WorkspaceRoot:  m.workspace,
		HomeDir:        m.home,
		TempRoot:       commandTempRoot,
		SensitivePaths: append([]string(nil), m.sensitivePaths...),
		SocketPaths:    append([]string(nil), m.socketPaths...),
		Git:            cloneGitLayout(m.git),
	}
	spec := CommandSpec{
		Command:        command,
		Directory:      m.workspace,
		Timeout:        timeout,
		MaxOutputChars: maxOutputChars,
	}
	if mode == ModeFullAccess {
		spec.Environment = os.Environ()
		return hostRunner{}.Run(ctx, spec, policy)
	}
	spec.Environment = m.safeEnvironment(network, commandTempRoot)
	return m.runner.Run(ctx, spec, policy)
}

func (m *Manager) StartProcess(ctx context.Context, spec ProcessSpec, modeOverride *Mode) (LongRunningProcess, error) {
	mode, network, capabilities, policyErr := m.snapshot(modeOverride)
	if err := preflightSnapshot(mode, capabilities, policyErr); err != nil {
		return nil, err
	}
	if strings.TrimSpace(spec.Program) == "" {
		return nil, fmt.Errorf("%w: program 不能为空", ErrPolicy)
	}
	if strings.TrimSpace(spec.Directory) == "" {
		spec.Directory = m.workspace
	}
	processTempRoot := m.tempRoot
	var cleanup func()
	if mode != ModeFullAccess {
		var err error
		processTempRoot, err = m.newProcessTempRoot("process-")
		if err != nil {
			return nil, fmt.Errorf("初始化长驻进程隔离目录: %w", err)
		}
		cleanup = func() { _ = os.RemoveAll(processTempRoot) }
	}
	policy := Policy{
		Mode:           mode,
		NetworkAccess:  network,
		WorkspaceRoot:  m.workspace,
		HomeDir:        m.home,
		TempRoot:       processTempRoot,
		SensitivePaths: append([]string(nil), m.sensitivePaths...),
		SocketPaths:    append([]string(nil), m.socketPaths...),
		Git:            cloneGitLayout(m.git),
	}
	baseEnvironment := os.Environ()
	runner := Runner(hostRunner{})
	if mode != ModeFullAccess {
		baseEnvironment = m.safeEnvironment(network, processTempRoot)
		runner = m.runner
	}
	environment, err := mergeEnvironment(baseEnvironment, spec.Environment)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, err
	}
	spec.Environment = environment
	prepared, err := runner.PrepareProcess(spec, policy)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, err
	}
	process, err := startManagedProcess(ctx, prepared, cleanup)
	if err != nil {
		return nil, err
	}
	process.startWatcher()
	return process, nil
}

func (m *Manager) newCommandTempRoot() (string, error) {
	return m.newProcessTempRoot("command-")
}

func (m *Manager) newProcessTempRoot(prefix string) (string, error) {
	root, err := os.MkdirTemp(m.tempRoot, prefix)
	if err != nil {
		return "", err
	}
	for _, name := range []string{"tmp", "cache", "run"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			_ = os.RemoveAll(root)
			return "", err
		}
	}
	return root, nil
}

func mergeEnvironment(base, extra []string) ([]string, error) {
	values := make(map[string]string, len(base)+len(extra))
	add := func(items []string, explicit bool) error {
		for _, item := range items {
			name, value, ok := strings.Cut(item, "=")
			if !ok || strings.TrimSpace(name) == "" || strings.Contains(name, "=") {
				if explicit {
					return fmt.Errorf("%w: 非法环境变量赋值", ErrPolicy)
				}
				continue
			}
			values[name] = value
		}
		return nil
	}
	if err := add(base, false); err != nil {
		return nil, err
	}
	if err := add(extra, true); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out, nil
}

func (m *Manager) snapshot(modeOverride *Mode) (Mode, bool, Capabilities, error) {
	for {
		m.mu.RLock()
		mode := m.mode
		if modeOverride != nil {
			mode = *modeOverride
		}
		network := effectiveNetworkAccess(mode, m.networkAccess)
		capabilities := m.capabilities
		probed := m.probed
		policyErr := m.policyErr
		m.mu.RUnlock()
		if mode == ModeFullAccess || probed {
			return mode, network, capabilities, policyErr
		}
		m.ensureProbed(context.Background())
	}
}

func effectiveNetworkAccess(mode Mode, configured bool) bool {
	return mode == ModeFullAccess || configured
}

func (m *Manager) safeEnvironment(networkAccess bool, tempRoots ...string) []string {
	tempRoot := m.tempRoot
	if len(tempRoots) > 0 && tempRoots[0] != "" {
		tempRoot = tempRoots[0]
	}
	builtins := map[string]bool{
		"PATH": true, "LANG": true, "TERM": true, "COLORTERM": true,
		"NO_COLOR": true, "CI": true, "USER": true, "LOGNAME": true,
		"SHELL": true, "GOROOT": true, "JAVA_HOME": true, "SDKROOT": true,
		"HOMEBREW_PREFIX": true,
	}
	for _, name := range m.allowedEnv {
		builtins[name] = true
	}
	values := map[string]string{}
	for _, item := range os.Environ() {
		name, value, ok := strings.Cut(item, "=")
		if !ok || (!builtins[name] && !strings.HasPrefix(name, "LC_")) {
			continue
		}
		values[name] = value
	}
	values["HOME"] = m.home
	values["TMPDIR"] = filepath.Join(tempRoot, "tmp")
	values["TMP"] = values["TMPDIR"]
	values["TEMP"] = values["TMPDIR"]
	values["XDG_CACHE_HOME"] = filepath.Join(tempRoot, "cache")
	values["XDG_RUNTIME_DIR"] = filepath.Join(tempRoot, "run")
	values["GOCACHE"] = filepath.Join(tempRoot, "cache", "go-build")
	values["npm_config_cache"] = filepath.Join(tempRoot, "cache", "npm")
	values["BRUCE_SANDBOX"] = m.capabilities.Backend
	if !networkAccess {
		values["BRUCE_SANDBOX_NETWORK_DISABLED"] = "1"
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

func protectedHostPaths(home string) ([]string, []string) {
	readDenied := []string{
		filepath.Join(home, ".ssh"), filepath.Join(home, ".gnupg"),
		filepath.Join(home, ".aws"), filepath.Join(home, ".azure"),
		filepath.Join(home, ".config", "gcloud"), filepath.Join(home, ".kube"),
		filepath.Join(home, ".docker", "config.json"), filepath.Join(home, ".netrc"),
		filepath.Join(home, ".git-credentials"), filepath.Join(home, ".npmrc"),
		filepath.Join(home, ".pypirc"), filepath.Join(home, ".config", "gh", "hosts.yml"),
		filepath.Join(home, ".config", "git", "credentials"),
		filepath.Join(home, "Library", "Keychains"),
	}
	sockets := []string{
		"/var/run/docker.sock", "/run/docker.sock",
		filepath.Join(home, ".docker", "run", "docker.sock"),
		filepath.Join(home, ".local", "share", "containers", "podman", "machine", "podman.sock"),
	}
	if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" {
		sockets = append(sockets,
			filepath.Join(runtimeDir, "docker.sock"),
			filepath.Join(runtimeDir, "podman", "podman.sock"),
			filepath.Join(runtimeDir, "bus"),
		)
	}
	for _, name := range []string{"SSH_AUTH_SOCK", "GPG_AGENT_INFO"} {
		if raw := strings.TrimSpace(os.Getenv(name)); raw != "" {
			path := strings.SplitN(raw, ":", 2)[0]
			if filepath.IsAbs(path) {
				sockets = append(sockets, filepath.Clean(path))
			}
		}
	}
	return uniqueCleanPaths(readDenied), uniqueCleanPaths(sockets)
}

func validateWritableWorkspace(workspace, home string) error {
	root := string(os.PathSeparator)
	if workspace == root || workspace == home || pathContains(workspace, home) {
		return fmt.Errorf("workspace-write 拒绝过宽 workspace: %s", workspace)
	}
	return nil
}

func canonicalAbsolute(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("路径不能为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return filepath.Clean(canonical), nil
	}
	return filepath.Clean(abs), nil
}

func uniqueCleanPaths(paths []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			continue
		}
		path = canonicalizePathAllowMissing(path)
		if seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func canonicalizePathAllowMissing(path string) string {
	path = filepath.Clean(path)
	current := path
	var suffix []string
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func pathContains(parent, child string) bool {
	if parent == child {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func cloneGitLayout(layout GitLayout) GitLayout {
	layout.WriteRoots = append([]string(nil), layout.WriteRoots...)
	layout.ProtectedPaths = append([]string(nil), layout.ProtectedPaths...)
	return layout
}
