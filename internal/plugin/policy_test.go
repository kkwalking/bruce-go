package plugin

import (
	"errors"
	"strings"
	"testing"
	"time"

	"bruce-go/internal/sandbox"
	"bruce-go/internal/tool"
)

func TestNewHostPolicyDefaultsToDeny(t *testing.T) {
	policy := NewHostPolicy()
	grant := policy.GrantFor("demo", Permissions())
	if len(grant.Granted) != 0 {
		t.Fatalf("NewHostPolicy() granted %v, want nothing", grant.Granted)
	}
	for _, permission := range Permissions() {
		if grant.Allows(permission) {
			t.Errorf("Allows(%q) = true on an empty policy", permission)
		}
	}
	if summary := grant.Summary(); !strings.Contains(summary, "none") {
		t.Errorf("Summary() = %q, want it to say nothing was granted", summary)
	}
}

func TestGrantForOnlyGrantsRequestedPermissions(t *testing.T) {
	policy := NewHostPolicy(PermissionFilesystemRead, PermissionStorage)
	grant := policy.GrantFor("demo", []Permission{PermissionFilesystemRead, PermissionNetwork})

	if !grant.Allows(PermissionFilesystemRead) {
		t.Error("a requested and allowed permission must be granted")
	}
	if grant.Allows(PermissionNetwork) {
		t.Error("a permission the policy does not allow must not be granted")
	}
	if grant.Allows(PermissionStorage) {
		t.Error("a permission the manifest did not request must not be granted")
	}
	if len(grant.Undeclared) != 1 || grant.Undeclared[0] != PermissionStorage {
		t.Errorf("Undeclared = %v, want [%s]", grant.Undeclared, PermissionStorage)
	}
	if !grant.Requested[PermissionNetwork] || !grant.Requested[PermissionFilesystemRead] {
		t.Errorf("Requested = %v, want the manifest's list", grant.Requested)
	}
}

func TestGrantForElevationWidensAndReportsUndeclared(t *testing.T) {
	policy := NewHostPolicy(PermissionNetwork)
	policy.AllowElevation = true
	grant := policy.GrantFor("demo", nil)

	if !grant.Allows(PermissionNetwork) {
		t.Error("AllowElevation must grant an allowed permission the manifest did not request")
	}
	if len(grant.Undeclared) != 1 || grant.Undeclared[0] != PermissionNetwork {
		t.Errorf("Undeclared = %v, want [%s] even when elevated", grant.Undeclared, PermissionNetwork)
	}
	// Elevation is not a bypass: an undeclared permission the policy does not
	// allow is still not granted.
	if grant.Allows(PermissionShell) || grant.Allows(PermissionFilesystemWrite) {
		t.Errorf("elevation granted permissions outside Allowed: %v", grant.Granted)
	}
}

func TestDeniedAlwaysWins(t *testing.T) {
	policy := NewHostPolicy(PermissionShell, PermissionNetwork)
	policy.Denied[PermissionShell] = true
	grant := policy.GrantFor("demo", []Permission{PermissionShell, PermissionNetwork})

	if grant.Allows(PermissionShell) {
		t.Error("Denied must override Allowed")
	}
	if !grant.Allows(PermissionNetwork) {
		t.Error("Denied must only affect the named permission")
	}

	// Denied also wins over a per-plugin widening and over elevation.
	policy.AllowElevation = true
	policy.PerPlugin["demo"] = map[Permission]bool{PermissionShell: true}
	elevated := policy.GrantFor("demo", []Permission{PermissionShell})
	if elevated.Allows(PermissionShell) {
		t.Error("Denied must override PerPlugin and AllowElevation")
	}
	if len(elevated.Undeclared) != 0 {
		t.Errorf("a denied permission must not be reported as undeclared: %v", elevated.Undeclared)
	}
}

func TestPerPluginOverridesAllowed(t *testing.T) {
	policy := NewHostPolicy(PermissionFilesystemRead, PermissionStorage)
	policy.PerPlugin["restricted"] = map[Permission]bool{}
	policy.PerPlugin["trusted"] = map[Permission]bool{PermissionFilesystemRead: true, PermissionShell: true}

	restricted := policy.GrantFor("restricted", Permissions())
	if len(restricted.Granted) != 0 {
		t.Errorf("a plugin listed with an empty set must get nothing, got %v", restricted.Granted)
	}

	trusted := policy.GrantFor("trusted", []Permission{PermissionShell, PermissionFilesystemRead})
	if !trusted.Allows(PermissionShell) {
		t.Error("PerPlugin must widen Allowed for a listed plugin")
	}
	if trusted.Allows(PermissionStorage) {
		t.Error("PerPlugin must replace Allowed, not union with it")
	}

	// A plugin that is not listed falls back to Allowed.
	fallback := policy.GrantFor("other", []Permission{PermissionStorage})
	if !fallback.Allows(PermissionStorage) {
		t.Error("an unlisted plugin must fall back to Allowed")
	}
	if fallback.Allows(PermissionShell) {
		t.Error("an unlisted plugin must not inherit another plugin's widening")
	}
}

func TestGrantAllowsFailsClosed(t *testing.T) {
	var zero Grant
	if zero.Allows(PermissionFilesystemRead) {
		t.Error("the zero Grant must allow nothing")
	}
	grant := Grant{Granted: map[Permission]bool{PermissionFilesystemRead: false}}
	if grant.Allows(PermissionFilesystemRead) {
		t.Error("a false entry must not be treated as granted")
	}
	if grant.Allows("") {
		t.Error("the empty permission must never be granted")
	}
	if grant.Allows(PermissionNetwork) {
		t.Error("an absent entry must not be granted")
	}
}

func TestGrantSummaryIsStableAndSorted(t *testing.T) {
	policy := NewHostPolicy(PermissionShell, PermissionFilesystemRead, PermissionNetwork)
	grant := policy.GrantFor("demo", []Permission{PermissionShell, PermissionFilesystemRead, PermissionNetwork})

	first := grant.Summary()
	if first != "demo: fs.read, net, shell" {
		t.Fatalf("Summary() = %q, want %q", first, "demo: fs.read, net, shell")
	}
	for i := 0; i < 50; i++ {
		if got := grant.Summary(); got != first {
			t.Fatalf("Summary() is not deterministic: %q then %q", first, got)
		}
	}
	if summary := policy.GrantFor("", nil).Summary(); !strings.HasPrefix(summary, "<unknown>:") {
		t.Errorf("Summary() = %q, want an <unknown> prefix for an empty plugin name", summary)
	}
}

func TestDenialErrorAndIsDenial(t *testing.T) {
	denial := &Denial{Plugin: "demo", Permission: PermissionNetwork, Reason: "not granted"}
	message := denial.Error()
	for _, want := range []string{"demo", "net", "not granted"} {
		if !strings.Contains(message, want) {
			t.Errorf("Error() = %q, missing %q", message, want)
		}
	}
	if !IsDenial(denial) {
		t.Error("IsDenial must recognize a *Denial")
	}
	if !IsDenial(NewError("demo", "", "run", StageInvoke, CategoryPermission, denial)) {
		t.Error("IsDenial must find a wrapped *Denial")
	}
	if IsDenial(errors.New("boom")) {
		t.Error("IsDenial must not match an unrelated error")
	}
	if !strings.Contains((&Denial{Plugin: "demo", Permission: PermissionNetwork}).Error(), "net") {
		t.Error("a Denial without a reason must still name the permission")
	}
}

func TestCheckCapabilityRejectsUngrantedCapability(t *testing.T) {
	policy := NewHostPolicy(PermissionFilesystemRead)
	grant := policy.GrantFor("demo", Permissions())

	allowed := tool.Policy{Capability: tool.Capability{FilesystemRead: true, WorkspaceScope: tool.ScopeWorkspace}}
	if err := grant.CheckCapability(allowed); err != nil {
		t.Fatalf("CheckCapability() = %v, want nil for a granted capability", err)
	}

	rejected := tool.Policy{Capability: tool.Capability{FilesystemWrite: true, WorkspaceScope: tool.ScopeWorkspace}}
	err := grant.CheckCapability(rejected)
	if err == nil {
		t.Fatal("CheckCapability must reject an ungranted capability")
	}
	if !IsDenial(err) {
		t.Fatalf("CheckCapability() = %v, want a *Denial", err)
	}
	var denial *Denial
	if !errors.As(err, &denial) || denial.Permission != PermissionFilesystemWrite || denial.Plugin != "demo" {
		t.Fatalf("denial = %+v, want plugin demo and permission fs.write", denial)
	}

	// A shell capability needs three permissions; a grant covering none of
	// them must be rejected.
	if err := grant.CheckCapability(tool.Policy{Capability: tool.Capability{Shell: true}}); err == nil {
		t.Error("CheckCapability must reject a shell capability without a shell grant")
	}
}

func TestCheckCapabilityRejectsHostScopeWithoutFullAccess(t *testing.T) {
	policy := NewHostPolicy(PermissionFilesystemRead, PermissionFilesystemWrite)
	grant := policy.GrantFor("demo", Permissions())

	// The permissions alone are not enough: host scope needs a sandbox mode
	// that can confine it.
	hostScope := tool.Policy{
		MinimumMode: sandbox.ModeWorkspaceWrite,
		Capability:  tool.Capability{FilesystemRead: true, WorkspaceScope: tool.ScopeHost},
	}
	err := grant.CheckCapability(hostScope)
	if err == nil {
		t.Fatal("CheckCapability must reject host scope in a restricted sandbox mode")
	}
	if !IsDenial(err) {
		t.Fatalf("CheckCapability() = %v, want a *Denial", err)
	}
	if !strings.Contains(err.Error(), string(sandbox.ModeFullAccess)) {
		t.Errorf("Error() = %q, want it to name the required mode", err.Error())
	}

	fullAccess := hostScope
	fullAccess.MinimumMode = sandbox.ModeFullAccess
	if err := grant.CheckCapability(fullAccess); err != nil {
		t.Errorf("CheckCapability() = %v, want nil for host scope under full access", err)
	}
}

func TestCapabilityPermissionsMapping(t *testing.T) {
	cases := []struct {
		name       string
		capability tool.Capability
		want       []Permission
	}{
		{"empty", tool.Capability{}, nil},
		{"read", tool.Capability{FilesystemRead: true}, []Permission{PermissionFilesystemRead}},
		{"write", tool.Capability{FilesystemWrite: true}, []Permission{PermissionFilesystemWrite}},
		{"network", tool.Capability{Network: true}, []Permission{PermissionNetwork}},
		{
			"shell implies both filesystem permissions",
			tool.Capability{Shell: true},
			[]Permission{PermissionFilesystemRead, PermissionFilesystemWrite, PermissionShell},
		},
		{
			"everything",
			tool.Capability{FilesystemRead: true, FilesystemWrite: true, Network: true, Shell: true},
			[]Permission{PermissionFilesystemRead, PermissionFilesystemWrite, PermissionNetwork, PermissionShell},
		},
		{
			"host scope alone implies nothing",
			tool.Capability{WorkspaceScope: tool.ScopeHost},
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CapabilityPermissions(tc.capability)
			if len(got) != len(tc.want) {
				t.Fatalf("CapabilityPermissions(%+v) = %v, want %v", tc.capability, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("CapabilityPermissions(%+v) = %v, want %v (sorted)", tc.capability, got, tc.want)
				}
			}
			for i := 1; i < len(got); i++ {
				if got[i-1] >= got[i] {
					t.Fatalf("result is not sorted: %v", got)
				}
			}
			// The result must not alias caller state.
			if len(got) > 0 {
				got[0] = "mutated"
				if again := CapabilityPermissions(tc.capability); again[0] == "mutated" {
					t.Fatal("CapabilityPermissions returned a shared slice")
				}
			}
		})
	}
}

func TestToolPolicyDerivation(t *testing.T) {
	cases := []struct {
		name             string
		capability       tool.Capability
		wantMode         sandbox.Mode
		wantApproval     bool
		wantRequiresNet  bool
		wantPermissionNo int
	}{
		{"none", tool.Capability{}, sandbox.ModeReadOnly, false, false, 0},
		{"read only", tool.Capability{FilesystemRead: true, WorkspaceScope: tool.ScopeWorkspace}, sandbox.ModeReadOnly, false, false, 1},
		{"write", tool.Capability{FilesystemWrite: true, WorkspaceScope: tool.ScopeWorkspace}, sandbox.ModeWorkspaceWrite, true, false, 1},
		{"shell", tool.Capability{Shell: true}, sandbox.ModeWorkspaceWrite, true, false, 3},
		{"network", tool.Capability{Network: true}, sandbox.ModeReadOnly, true, true, 1},
		{"read and network", tool.Capability{FilesystemRead: true, Network: true}, sandbox.ModeReadOnly, true, true, 2},
		{"write and network", tool.Capability{FilesystemWrite: true, Network: true}, sandbox.ModeWorkspaceWrite, true, true, 2},
		{
			"all capabilities",
			tool.Capability{FilesystemRead: true, FilesystemWrite: true, Network: true, Shell: true, WorkspaceScope: tool.ScopeWorkspace},
			sandbox.ModeWorkspaceWrite, true, true, 4,
		},
		{
			"host scope read",
			tool.Capability{FilesystemRead: true, WorkspaceScope: tool.ScopeHost},
			sandbox.ModeReadOnly, false, false, 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			timeout := 3 * time.Second
			policy := ToolPolicy(tool.SourcePlugin, tc.capability, true, tool.RiskMedium, timeout, "because")

			if policy.Source != tool.SourcePlugin {
				t.Errorf("Source = %q, want %q", policy.Source, tool.SourcePlugin)
			}
			if policy.MinimumMode != tc.wantMode {
				t.Errorf("MinimumMode = %q, want %q", policy.MinimumMode, tc.wantMode)
			}
			if policy.RequiresApproval != tc.wantApproval {
				t.Errorf("RequiresApproval = %v, want %v", policy.RequiresApproval, tc.wantApproval)
			}
			if policy.RequiresNetwork != tc.wantRequiresNet {
				t.Errorf("RequiresNetwork = %v, want %v", policy.RequiresNetwork, tc.wantRequiresNet)
			}
			if !policy.ParallelSafe {
				t.Error("ParallelSafe must carry the declaration")
			}
			if policy.Risk != tool.RiskMedium {
				t.Errorf("Risk = %q, want %q", policy.Risk, tool.RiskMedium)
			}
			if policy.Timeout != timeout {
				t.Errorf("Timeout = %v, want %v", policy.Timeout, timeout)
			}
			if policy.ApprovalReason != "because" {
				t.Errorf("ApprovalReason = %q, want %q", policy.ApprovalReason, "because")
			}
			if policy.Capability != tc.capability {
				t.Errorf("Capability = %+v, want %+v", policy.Capability, tc.capability)
			}
			// The tool layer must agree with the derivation.
			if policy.NeedsApproval() != tc.wantApproval {
				t.Errorf("NeedsApproval() = %v, want %v", policy.NeedsApproval(), tc.wantApproval)
			}
			if got := len(CapabilityPermissions(tc.capability)); got != tc.wantPermissionNo {
				t.Errorf("len(CapabilityPermissions()) = %d, want %d", got, tc.wantPermissionNo)
			}
		})
	}
}

func TestToolPolicyNeverInheritsSafetyByOmission(t *testing.T) {
	// A plugin tool that declares a mutating capability must ask for approval
	// even when the caller passes an empty reason and no explicit flag.
	for _, capability := range []tool.Capability{
		{FilesystemWrite: true},
		{Shell: true},
		{Network: true},
	} {
		policy := ToolPolicy(tool.SourcePlugin, capability, false, tool.RiskHigh, 0, "")
		if !policy.RequiresApproval {
			t.Errorf("ToolPolicy(%+v).RequiresApproval = false, want true", capability)
		}
	}
	// A read-only plugin tool does not need approval, and stays read-only.
	readOnly := ToolPolicy(tool.SourcePlugin, tool.Capability{FilesystemRead: true, WorkspaceScope: tool.ScopeWorkspace}, false, tool.RiskLow, 0, "")
	if readOnly.RequiresApproval {
		t.Error("a read-only plugin tool must not require approval")
	}
	if readOnly.MinimumMode != sandbox.ModeReadOnly {
		t.Errorf("MinimumMode = %q, want %q", readOnly.MinimumMode, sandbox.ModeReadOnly)
	}
}

func TestSandboxAllowsFailsClosed(t *testing.T) {
	available := sandbox.Status{
		Mode:          sandbox.ModeWorkspaceWrite,
		NetworkAccess: true,
		Capabilities:  sandbox.Capabilities{Backend: "seatbelt", Available: true},
	}
	cases := []struct {
		name       string
		status     sandbox.Status
		capability tool.Capability
		want       bool
		wantReason string
	}{
		{
			"unknown mode denies everything",
			sandbox.Status{Mode: "", Capabilities: sandbox.Capabilities{Backend: "seatbelt", Available: true}},
			tool.Capability{FilesystemRead: true},
			false, "not recognized",
		},
		{
			"unknown mode still allows the empty capability",
			sandbox.Status{Mode: ""},
			tool.Capability{},
			true, "",
		},
		{
			"unavailable backend denies a read",
			sandbox.Status{Mode: sandbox.ModeReadOnly, Capabilities: sandbox.Capabilities{Backend: "seatbelt", Reason: "probe failed"}},
			tool.Capability{FilesystemRead: true},
			false, "probe failed",
		},
		{
			"unavailable backend denies a write",
			sandbox.Status{Mode: sandbox.ModeWorkspaceWrite, Capabilities: sandbox.Capabilities{Backend: "bubblewrap"}},
			tool.Capability{FilesystemWrite: true},
			false, "bubblewrap",
		},
		{
			"unavailable backend denies shell",
			sandbox.Status{Mode: sandbox.ModeWorkspaceWrite, Capabilities: sandbox.Capabilities{Backend: "none"}},
			tool.Capability{Shell: true},
			false, "unavailable",
		},
		{
			"unavailable backend denies network",
			sandbox.Status{Mode: sandbox.ModeWorkspaceWrite, Capabilities: sandbox.Capabilities{Backend: "none"}},
			tool.Capability{Network: true},
			false, "unavailable",
		},
		{
			"unavailable backend still allows the empty capability",
			sandbox.Status{Mode: sandbox.ModeWorkspaceWrite, Capabilities: sandbox.Capabilities{Backend: "none"}},
			tool.Capability{},
			true, "",
		},
		{
			"read-only denies writes",
			sandbox.Status{Mode: sandbox.ModeReadOnly, Capabilities: sandbox.Capabilities{Backend: "seatbelt", Available: true}},
			tool.Capability{FilesystemWrite: true},
			false, "filesystem writes",
		},
		{
			"read-only denies shell",
			sandbox.Status{Mode: sandbox.ModeReadOnly, Capabilities: sandbox.Capabilities{Backend: "seatbelt", Available: true}},
			tool.Capability{Shell: true},
			false, "shell execution",
		},
		{
			"read-only allows a read",
			sandbox.Status{Mode: sandbox.ModeReadOnly, Capabilities: sandbox.Capabilities{Backend: "seatbelt", Available: true}},
			tool.Capability{FilesystemRead: true, WorkspaceScope: tool.ScopeWorkspace},
			true, "",
		},
		{
			"disabled network denies a network capability",
			sandbox.Status{Mode: sandbox.ModeWorkspaceWrite, Capabilities: sandbox.Capabilities{Backend: "seatbelt", Available: true}},
			tool.Capability{Network: true},
			false, "network access is disabled",
		},
		{
			"host scope is denied outside full access",
			sandbox.Status{Mode: sandbox.ModeWorkspaceWrite, NetworkAccess: true, Capabilities: sandbox.Capabilities{Backend: "seatbelt", Available: true}},
			tool.Capability{FilesystemRead: true, WorkspaceScope: tool.ScopeHost},
			false, "full-access",
		},
		{
			"unknown workspace scope is denied",
			available,
			tool.Capability{FilesystemRead: true, WorkspaceScope: tool.WorkspaceScope("galaxy")},
			false, "not recognized",
		},
		{
			"full access allows host scope",
			sandbox.Status{Mode: sandbox.ModeFullAccess, NetworkAccess: true},
			tool.Capability{FilesystemRead: true, WorkspaceScope: tool.ScopeHost},
			true, "",
		},
		{
			"full access allows shell and network",
			sandbox.Status{Mode: sandbox.ModeFullAccess, NetworkAccess: true},
			tool.Capability{FilesystemRead: true, FilesystemWrite: true, Shell: true, Network: true, WorkspaceScope: tool.ScopeWorkspace},
			true, "",
		},
		{
			"workspace-write allows a write",
			available,
			tool.Capability{FilesystemWrite: true, WorkspaceScope: tool.ScopeWorkspace},
			true, "",
		},
		{
			"workspace-write allows network when enabled",
			available,
			tool.Capability{Network: true},
			true, "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowed, reason := SandboxAllows(tc.status, tc.capability)
			if allowed != tc.want {
				t.Fatalf("SandboxAllows() = %v (%q), want %v", allowed, reason, tc.want)
			}
			if tc.want {
				if reason != "" {
					t.Errorf("a permitted capability must not carry a reason, got %q", reason)
				}
				return
			}
			if strings.TrimSpace(reason) == "" {
				t.Fatal("a denial must carry a readable reason")
			}
			if tc.wantReason != "" && !strings.Contains(reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", reason, tc.wantReason)
			}
		})
	}
}
