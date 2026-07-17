package sandbox

import (
	"context"
	"errors"
	"time"
)

type Mode string

const (
	ModeReadOnly       Mode = "read-only"
	ModeWorkspaceWrite Mode = "workspace-write"
	ModeFullAccess     Mode = "full-access"
)

func ParseMode(raw string) (Mode, error) {
	switch Mode(raw) {
	case ModeReadOnly, ModeWorkspaceWrite, ModeFullAccess:
		return Mode(raw), nil
	default:
		return "", errors.New("未知 sandbox mode: " + raw)
	}
}

type GitLayout struct {
	MarkerPath     string
	GitDir         string
	CommonDir      string
	WriteRoots     []string
	ProtectedPaths []string
}

type Policy struct {
	Mode           Mode
	NetworkAccess  bool
	WorkspaceRoot  string
	HomeDir        string
	TempRoot       string
	SensitivePaths []string
	SocketPaths    []string
	Git            GitLayout
}

type CommandSpec struct {
	Command        string
	Directory      string
	Environment    []string
	Timeout        time.Duration
	MaxOutputChars int
}

type RunResult struct {
	Output    string
	ExitCode  int
	TimedOut  bool
	Canceled  bool
	Truncated bool
}

type Capabilities struct {
	Backend   string
	Available bool
	Reason    string
}

type Status struct {
	Mode                    Mode
	NetworkAccess           bool
	ConfiguredNetworkAccess bool
	Capabilities            Capabilities
}

type Runner interface {
	Name() string
	Probe(ctx context.Context) Capabilities
	Run(ctx context.Context, spec CommandSpec, policy Policy) (RunResult, error)
}

var (
	ErrUnavailable = errors.New("sandbox backend 不可用")
	ErrPolicy      = errors.New("sandbox 策略拒绝")
)
