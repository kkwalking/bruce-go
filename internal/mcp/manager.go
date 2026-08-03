package mcp

import (
	"context"
	"errors"
	"sort"
	"sync"

	"bruce-go/internal/config"
	"bruce-go/internal/sandbox"
)

type ServerStatus struct {
	Name             string
	Enabled          bool
	Ready            bool
	ToolCount        int
	BlockedToolCount int
	Transport        string
	Enforcement      string
	Generation       uint64
	BlockedReason    string
	Error            string
}

type Manager struct {
	mu            sync.RWMutex
	transitionMu  sync.Mutex
	servers       map[string]*Server
	workspace     string
	factory       TransportFactory
	sandbox       *sandbox.Manager
	transitioning bool
	generation    uint64
	started       bool
	calls         sync.WaitGroup
}

type Server struct {
	Name      string
	Config    config.MCPServerSetting
	Enabled   bool
	Ready     bool
	Tools     []Tool
	LastError string
	Blocked   string
	transport Transport
}

func NewManager(settings config.MCPSettings, workspace string) *Manager {
	m := &Manager{servers: map[string]*Server{}, workspace: workspace, generation: 1}
	for name, cfg := range settings.Servers {
		server := &Server{Name: name, Config: cfg, Enabled: !cfg.Disabled}
		m.servers[name] = server
	}
	return m
}

func (m *Manager) WithSandbox(manager *sandbox.Manager) *Manager {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandbox = manager
	return m
}

func (m *Manager) WithFactory(factory TransportFactory) *Manager {
	m.mu.Lock()
	defer m.mu.Unlock()
	if factory != nil {
		m.factory = factory
	}
	return m
}

func (m *Manager) StartEnabled(ctx context.Context) {
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	m.mu.Lock()
	m.started = true
	m.mu.Unlock()
	m.startEnabled(ctx)
}

func (m *Manager) startEnabled(ctx context.Context) {
	names := m.Names()
	for _, name := range names {
		m.mu.RLock()
		enabled := m.servers[name] != nil && m.servers[name].Enabled
		m.mu.RUnlock()
		if !enabled {
			continue
		}
		_ = m.enable(ctx, name)
	}
}

func (m *Manager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.servers))
	for name := range m.servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *Manager) Status() []ServerStatus {
	policy := m.sandboxStatus()
	m.mu.RLock()
	defer m.mu.RUnlock()
	statuses := make([]ServerStatus, 0, len(m.servers))
	for _, server := range m.servers {
		available := 0
		for _, candidate := range server.Tools {
			if toolAllowed(server.Config, candidate.Name, policy) {
				available++
			}
		}
		blockedTools := len(server.Tools) - available
		blockedReason := server.Blocked
		if blockedTools > 0 && blockedReason == "" {
			blockedReason = "部分工具因当前 sandbox policy 或缺少精确 toolAccess 被阻止"
		}
		statuses = append(statuses, ServerStatus{
			Name:             server.Name,
			Enabled:          server.Enabled,
			Ready:            server.Ready,
			ToolCount:        available,
			BlockedToolCount: blockedTools,
			Transport:        normalizedTransport(server.Config.Type),
			Enforcement:      enforcementText(server.Config, policy),
			Generation:       m.generation,
			BlockedReason:    blockedReason,
			Error:            server.LastError,
		})
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	return statuses
}

func (m *Manager) Enable(ctx context.Context, name string) error {
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	m.mu.Lock()
	m.started = true
	m.mu.Unlock()
	return m.enable(ctx, name)
}

func (m *Manager) enable(ctx context.Context, name string) error {
	m.mu.Lock()
	server := m.servers[name]
	if server == nil {
		m.mu.Unlock()
		return errors.New("未知 MCP server: " + name)
	}
	server.Enabled = true
	if server.Ready {
		m.mu.Unlock()
		return nil
	}
	cfg := server.Config
	factory := m.factory
	sandboxManager := m.sandbox
	workspace := m.workspace
	m.mu.Unlock()

	policy := m.sandboxStatus()
	if reason := serverStartBlock(cfg, policy); reason != "" {
		m.recordBlocked(name, reason)
		return nil
	}
	var transport Transport
	var err error
	if factory != nil {
		transport, err = factory(ctx, name, cfg, workspace)
	} else {
		transport, err = defaultTransportFactory(ctx, name, cfg, workspace, sandboxManager)
	}
	if err != nil {
		m.recordError(name, err)
		return err
	}
	tools, err := initializeAndList(ctx, transport)
	if err != nil {
		_ = transport.Close()
		m.recordError(name, err)
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	server = m.servers[name]
	if server == nil {
		_ = transport.Close()
		return errors.New("未知 MCP server: " + name)
	}
	if server.transport != nil {
		_ = server.transport.Close()
	}
	server.transport = transport
	server.Tools = tools
	server.Ready = true
	server.Enabled = true
	server.Blocked = ""
	server.LastError = ""
	return nil
}

func (m *Manager) Disable(name string) error {
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	return m.disable(name)
}

func (m *Manager) disable(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	server := m.servers[name]
	if server == nil {
		return errors.New("未知 MCP server: " + name)
	}
	if server.transport != nil {
		_ = server.transport.Close()
	}
	server.transport = nil
	server.Enabled = false
	server.Ready = false
	server.Tools = nil
	server.Blocked = ""
	return nil
}

func (m *Manager) Restart(ctx context.Context, name string) error {
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	if err := m.disable(name); err != nil {
		return err
	}
	return m.enable(ctx, name)
}

func (m *Manager) Logs(name string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	server := m.servers[name]
	if server == nil || server.transport == nil {
		return nil
	}
	return server.transport.Logs()
}

func (m *Manager) Reconfigure(ctx context.Context, apply func() error) error {
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()

	m.mu.Lock()
	m.transitioning = true
	m.generation++
	started := m.started
	transports := make([]Transport, 0, len(m.servers))
	for _, server := range m.servers {
		if server.transport != nil {
			transports = append(transports, server.transport)
		}
		server.transport = nil
		server.Ready = false
		server.Tools = nil
		server.Blocked = ""
	}
	m.mu.Unlock()

	for _, transport := range transports {
		_ = transport.Close()
	}
	m.calls.Wait()

	var applyErr error
	if apply != nil {
		applyErr = apply()
	}
	if started {
		m.startEnabled(ctx)
	}
	m.mu.Lock()
	m.transitioning = false
	m.generation++
	m.mu.Unlock()
	return applyErr
}

func (m *Manager) Close() error {
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	m.mu.Lock()
	m.transitioning = true
	m.started = false
	transports := make([]Transport, 0, len(m.servers))
	for _, server := range m.servers {
		if server.transport != nil {
			transports = append(transports, server.transport)
		}
		server.transport = nil
		server.Ready = false
		server.Tools = nil
	}
	m.mu.Unlock()
	var first error
	for _, transport := range transports {
		if err := transport.Close(); err != nil && first == nil {
			first = err
		}
	}
	m.calls.Wait()
	return first
}

func (m *Manager) sandboxStatus() sandbox.Status {
	m.mu.RLock()
	manager := m.sandbox
	m.mu.RUnlock()
	if manager == nil {
		return sandbox.Status{
			Mode:                    sandbox.ModeFullAccess,
			NetworkAccess:           true,
			ConfiguredNetworkAccess: true,
			Capabilities:            sandbox.Capabilities{Backend: "none", Available: true},
		}
	}
	return manager.Status()
}

func (m *Manager) recordError(name string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if server := m.servers[name]; server != nil {
		server.LastError = err.Error()
		server.Blocked = ""
		server.Ready = false
	}
}

func (m *Manager) recordBlocked(name, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if server := m.servers[name]; server != nil {
		server.LastError = ""
		server.Blocked = reason
		server.Ready = false
		server.Tools = nil
	}
}
