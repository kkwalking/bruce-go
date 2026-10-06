package plugin

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"bruce-go/internal/sandbox"
	"bruce-go/internal/tool"
)

// HostPolicy is the final authority on what a plugin may do. A manifest's
// permissions are a request; this decides the grant.
//
// The zero value grants nothing. A permission reaches a plugin only when this
// policy names it, the manifest asked for it, and no deny rule overrides it.
type HostPolicy struct {
	// Allowed lists the permissions the host grants to every plugin.
	Allowed map[Permission]bool
	// PerPlugin narrows or widens Allowed for one plugin name. A plugin not
	// listed falls back to Allowed.
	PerPlugin map[string]map[Permission]bool
	// Denied always wins, for every plugin.
	Denied map[Permission]bool
	// AllowElevation, when false (the default), makes it impossible for a
	// plugin to receive a permission it did not declare in its manifest.
	AllowElevation bool
}

// NewHostPolicy builds a policy that grants exactly the named permissions, to
// every plugin that requests them.
//
// Called with no arguments it grants nothing at all: a plugin system whose
// default is "allow" is one manifest typo away from handing a third-party
// script the network and the filesystem.
func NewHostPolicy(allowed ...Permission) HostPolicy {
	policy := HostPolicy{
		Allowed:   make(map[Permission]bool, len(allowed)),
		PerPlugin: map[string]map[Permission]bool{},
		Denied:    map[Permission]bool{},
	}
	for _, permission := range allowed {
		if permission == "" {
			continue
		}
		policy.Allowed[permission] = true
	}
	return policy
}

// Grant is the effective permission set of one plugin.
type Grant struct {
	Plugin    string
	Granted   map[Permission]bool
	Requested map[Permission]bool
	// Undeclared lists the permissions the policy would grant but the manifest
	// never asked for. It is reported by /plugin status: a host policy that
	// offers more than a plugin requests is worth seeing, even when elevation
	// is disabled and nothing was actually granted.
	Undeclared []Permission
}

// GrantFor resolves the effective permissions of one plugin.
//
// The rules are, in order: Denied always wins; PerPlugin replaces Allowed for
// a listed plugin (a listed plugin with an empty set gets nothing); and, unless
// AllowElevation is set, a permission the manifest did not request is never
// granted.
func (p HostPolicy) GrantFor(plugin string, requested []Permission) Grant {
	grant := Grant{
		Plugin:    plugin,
		Granted:   map[Permission]bool{},
		Requested: make(map[Permission]bool, len(requested)),
	}
	for _, permission := range requested {
		if permission == "" {
			continue
		}
		grant.Requested[permission] = true
	}

	candidates := p.Allowed
	if perPlugin, listed := p.PerPlugin[plugin]; listed {
		candidates = perPlugin
	}
	for permission, allowed := range candidates {
		if !allowed || permission == "" {
			continue
		}
		if p.Denied[permission] {
			continue
		}
		if !grant.Requested[permission] {
			// The policy allows it, the manifest did not ask for it.
			grant.Undeclared = append(grant.Undeclared, permission)
			if !p.AllowElevation {
				continue
			}
		}
		grant.Granted[permission] = true
	}
	sortPermissions(grant.Undeclared)
	return grant
}

// Allows reports whether the grant covers a permission.
//
// It fails closed: the zero Grant allows nothing, and an empty permission is
// never granted.
func (g Grant) Allows(permission Permission) bool {
	if permission == "" {
		return false
	}
	return g.Granted[permission]
}

// Summary renders the granted permissions as a stable, sorted, human readable
// list for /plugin status output.
func (g Grant) Summary() string {
	name := strings.TrimSpace(g.Plugin)
	if name == "" {
		name = "<unknown>"
	}
	granted := sortedPermissions(g.Granted)
	if len(granted) == 0 {
		return name + ": none"
	}
	names := make([]string, 0, len(granted))
	for _, permission := range granted {
		names = append(names, string(permission))
	}
	return name + ": " + strings.Join(names, ", ")
}

// Denial is returned when a plugin asks for something it was not granted.
type Denial struct {
	Plugin     string
	Permission Permission
	Reason     string
}

// Compile-time proof that a Denial is usable as an error on its own, without
// the plugin error wrapper.
var _ error = (*Denial)(nil)

func (d *Denial) Error() string {
	name := strings.TrimSpace(d.Plugin)
	if name == "" {
		name = "<unknown>"
	}
	permission := string(d.Permission)
	if permission == "" {
		permission = "<none>"
	}
	if reason := Redact(d.Reason); reason != "" {
		return fmt.Sprintf("plugin %q was denied permission %q: %s", name, permission, reason)
	}
	return fmt.Sprintf("plugin %q was denied permission %q", name, permission)
}

// IsDenial reports whether err is (or wraps) a *Denial.
func IsDenial(err error) bool {
	var target *Denial
	return errors.As(err, &target)
}

// CheckCapability maps a tool.Policy's declared capability onto the
// permissions it needs, and verifies the grant covers all of them.
//
// It also rejects a host-scoped capability that is not paired with
// sandbox.ModeFullAccess: the sandbox is the only thing that can confine a
// capability reaching outside the workspace, so a tool policy that asks for
// host scope while requiring a restricted mode is internally inconsistent and
// must not be registered.
func (g Grant) CheckCapability(policy tool.Policy) error {
	for _, permission := range CapabilityPermissions(policy.Capability) {
		if g.Allows(permission) {
			continue
		}
		return &Denial{
			Plugin:     g.Plugin,
			Permission: permission,
			Reason:     fmt.Sprintf("the plugin tool requires %q, which the host policy did not grant", string(permission)),
		}
	}
	if policy.Capability.WorkspaceScope == tool.ScopeHost && policy.MinimumMode != sandbox.ModeFullAccess {
		return &Denial{
			Plugin:     g.Plugin,
			Permission: hostScopePermission(policy.Capability),
			Reason: fmt.Sprintf(
				"workspace scope %q needs sandbox mode %q, but the tool policy requires %q",
				string(tool.ScopeHost), string(sandbox.ModeFullAccess), string(policy.MinimumMode),
			),
		}
	}
	return nil
}

// hostScopePermission names the permission a host-scoped capability is
// reported against, or "" when the capability declares no filesystem
// permission to widen. Scope is not itself a permission, so a denial for a
// scope-only capability carries its explanation in Reason rather than
// inventing a permission the tool never declared.
func hostScopePermission(capability tool.Capability) Permission {
	switch {
	case capability.FilesystemWrite || capability.Shell:
		return PermissionFilesystemWrite
	case capability.FilesystemRead:
		return PermissionFilesystemRead
	default:
		return ""
	}
}

// CapabilityPermissions returns the permissions implied by a capability set,
// sorted. A shell capability implies both filesystem permissions; a network
// capability implies PermissionNetwork; ScopeHost implies nothing extra by
// itself but is reported by CheckCapability when the sandbox cannot allow it.
//
// The result is nil when the capability implies nothing, and it is always a
// fresh slice: callers may keep or reorder it.
func CapabilityPermissions(capability tool.Capability) []Permission {
	implied := map[Permission]bool{}
	if capability.FilesystemRead {
		implied[PermissionFilesystemRead] = true
	}
	if capability.FilesystemWrite {
		implied[PermissionFilesystemWrite] = true
	}
	if capability.Shell {
		// A shell runs arbitrary programs, so it can read and write whatever
		// the sandbox would otherwise allow: asking for shell without asking
		// for the filesystem is not a smaller request.
		implied[PermissionFilesystemRead] = true
		implied[PermissionFilesystemWrite] = true
		implied[PermissionShell] = true
	}
	if capability.Network {
		implied[PermissionNetwork] = true
	}
	return sortedPermissions(implied)
}

// ToolPolicy derives the tool.Policy of a plugin tool from its declaration.
// minimumMode must be the sandbox mode required by the granted permissions:
//   - no capability                       -> sandbox.ModeReadOnly
//   - FilesystemWrite or Shell            -> sandbox.ModeWorkspaceWrite
//   - Network                             -> sandbox.ModeReadOnly
//
// RequiresApproval must be true whenever any of FilesystemWrite, Shell or
// Network is declared, and false otherwise.
func ToolPolicy(source tool.Source, capability tool.Capability, parallelSafe bool, risk tool.Risk, timeout time.Duration, approvalReason string) tool.Policy {
	return tool.Policy{
		Source:           source,
		MinimumMode:      minimumModeFor(capability),
		RequiresNetwork:  capability.Network,
		ParallelSafe:     parallelSafe,
		Capability:       capability,
		RequiresApproval: requiresApprovalFor(capability),
		ApprovalReason:   approvalReason,
		Risk:             risk,
		Timeout:          timeout,
	}
}

// minimumModeFor is the sandbox floor a capability needs. Reading needs no
// write access, so everything that does not mutate or escape stays read-only.
func minimumModeFor(capability tool.Capability) sandbox.Mode {
	if capability.FilesystemWrite || capability.Shell {
		return sandbox.ModeWorkspaceWrite
	}
	return sandbox.ModeReadOnly
}

// requiresApprovalFor treats any mutating or exfiltrating capability as
// requiring human approval. A plugin tool never inherits "safe" by omission.
func requiresApprovalFor(capability tool.Capability) bool {
	return capability.FilesystemWrite || capability.Shell || capability.Network
}

// SandboxAllows reports whether the current sandbox status permits a
// capability, and why not when it does not.
//
// It fails closed. An unrecognized mode, a restricted mode whose backend is
// unavailable or was never probed, a disabled network, and a host-scoped
// capability outside full access all deny. Only the empty capability set is
// always permitted, because it asks for nothing.
func SandboxAllows(status sandbox.Status, capability tool.Capability) (bool, string) {
	if capability.Empty() {
		return true, ""
	}
	switch status.Mode {
	case sandbox.ModeReadOnly, sandbox.ModeWorkspaceWrite, sandbox.ModeFullAccess:
	default:
		return false, fmt.Sprintf("sandbox mode %q is not recognized; no capability can be authorized", string(status.Mode))
	}
	switch capability.WorkspaceScope {
	case tool.ScopeNone, tool.ScopeWorkspace, tool.ScopeHost:
	default:
		return false, fmt.Sprintf("workspace scope %q is not recognized; no capability can be authorized", string(capability.WorkspaceScope))
	}
	// Full access runs without a native backend by design (see
	// sandbox.Manager.Preflight), so backend availability only constrains the
	// restricted modes.
	if status.Mode != sandbox.ModeFullAccess && !status.Capabilities.Available {
		backend := status.Capabilities.Backend
		if backend == "" {
			backend = "unknown"
		}
		reason := status.Capabilities.Reason
		if reason == "" {
			reason = "the backend is unavailable or has not been probed"
		}
		return false, fmt.Sprintf("sandbox backend %q is unavailable: %s", backend, reason)
	}
	if capability.Network && !status.NetworkAccess {
		return false, "sandbox network access is disabled"
	}
	if capability.WorkspaceScope == tool.ScopeHost && status.Mode != sandbox.ModeFullAccess {
		return false, fmt.Sprintf(
			"sandbox mode %q cannot confine a capability scoped to %q; %q is required",
			string(status.Mode), string(tool.ScopeHost), string(sandbox.ModeFullAccess),
		)
	}
	if status.Mode == sandbox.ModeReadOnly {
		if capability.FilesystemWrite {
			return false, fmt.Sprintf("sandbox mode %q does not permit filesystem writes", string(status.Mode))
		}
		if capability.Shell {
			return false, fmt.Sprintf("sandbox mode %q does not permit shell execution", string(status.Mode))
		}
	}
	return true, ""
}

// sortedPermissions returns the enabled entries of a permission set in a
// stable order, or nil when there are none.
func sortedPermissions(set map[Permission]bool) []Permission {
	if len(set) == 0 {
		return nil
	}
	permissions := make([]Permission, 0, len(set))
	for permission, enabled := range set {
		if enabled && permission != "" {
			permissions = append(permissions, permission)
		}
	}
	if len(permissions) == 0 {
		return nil
	}
	sortPermissions(permissions)
	return permissions
}

// sortPermissions sorts in place, so that every rendered permission list is
// byte-identical between calls and between runs.
func sortPermissions(permissions []Permission) {
	sort.Slice(permissions, func(i, j int) bool { return permissions[i] < permissions[j] })
}
