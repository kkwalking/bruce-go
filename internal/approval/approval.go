package approval

import (
	"context"
	"strings"
	"sync"
)

type Decision string

const (
	Approved    Decision = "approved"
	ApprovedAll Decision = "approved_all"
	Modified    Decision = "modified"
	Rejected    Decision = "rejected"
	Skipped     Decision = "skipped"
)

type Result struct {
	Decision  Decision
	Reason    string
	Arguments string
}

func Approve() Result    { return Result{Decision: Approved} }
func ApproveAll() Result { return Result{Decision: ApprovedAll} }
func Reject(reason string) Result {
	return Result{Decision: Rejected, Reason: reason}
}
func Skip() Result { return Result{Decision: Skipped} }

func (r Result) IsRejected() bool { return r.Decision == Rejected }
func (r Result) IsSkipped() bool  { return r.Decision == Skipped }

func (r Result) EffectiveArguments(original string) string {
	if r.Decision == Modified && strings.TrimSpace(r.Arguments) != "" {
		return r.Arguments
	}
	return original
}

type Handler interface {
	Enabled() bool
	SetEnabled(bool)
	Request(context.Context, Request) (Result, error)
	ClearApprovedAll()
}

type AutoHandler struct {
	mu      sync.RWMutex
	enabled bool
	result  Result
}

func NewAutoHandler(enabled bool, result Result) *AutoHandler {
	if result.Decision == "" {
		result = Approve()
	}
	return &AutoHandler{enabled: enabled, result: result}
}

func (h *AutoHandler) Enabled() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.enabled
}

func (h *AutoHandler) SetEnabled(v bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.enabled = v
}

func (h *AutoHandler) ClearApprovedAll() {}
func (h *AutoHandler) Request(ctx context.Context, _ Request) (Result, error) {
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	default:
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.result, nil
}

// Request describes one invocation waiting for human approval.
//
// Every field is supplied by the caller that holds the tool's declared
// policy. Nothing here is derived from the tool name: since plugins exist, a
// tool chooses its own name, so the name cannot be trusted to describe risk.
type Request struct {
	ToolName        string
	Arguments       string
	DangerLevel     string
	RiskDescription string
}

// NewRequest builds the request for a tool invocation, using the danger level
// and reason the caller derived from the tool's declared policy.
func NewRequest(toolName, args, dangerLevel, riskDescription string) Request {
	return Request{
		ToolName:        toolName,
		Arguments:       args,
		DangerLevel:     dangerLevel,
		RiskDescription: riskDescription,
	}
}
