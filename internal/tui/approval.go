package tui

import (
	"context"
	"sync"
	"sync/atomic"

	tea "github.com/charmbracelet/bubbletea"

	"bruce-go/internal/approval"
)

type approvalRequestMsg struct {
	ID      uint64
	Request approval.Request
	Reply   chan approval.Result
}

type approvalCanceledMsg struct {
	ID uint64
}

type tuiApprovalHandler struct {
	mu          sync.RWMutex
	enabled     bool
	approvedAll bool
	program     *tea.Program
	requestGate chan struct{}
	nextID      atomic.Uint64
}

func newTUIApprovalHandler() *tuiApprovalHandler {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return &tuiApprovalHandler{enabled: false, requestGate: gate}
}

func (h *tuiApprovalHandler) SetProgram(program *tea.Program) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.program = program
}

func (h *tuiApprovalHandler) Enabled() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.enabled
}

func (h *tuiApprovalHandler) SetEnabled(v bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.enabled = v
	if !v {
		h.approvedAll = false
	}
}

func (h *tuiApprovalHandler) ClearApprovedAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.approvedAll = false
}

func (h *tuiApprovalHandler) Request(ctx context.Context, request approval.Request) (approval.Result, error) {
	select {
	case <-ctx.Done():
		return approval.Result{}, ctx.Err()
	case <-h.requestGate:
	}
	defer func() { h.requestGate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return approval.Result{}, err
	}

	h.mu.RLock()
	enabled := h.enabled
	approvedAll := h.approvedAll
	program := h.program
	h.mu.RUnlock()
	if !enabled || approvedAll {
		return approval.Approve(), nil
	}
	if program == nil {
		return approval.Approve(), nil
	}
	id := h.nextID.Add(1)
	reply := make(chan approval.Result, 1)
	program.Send(approvalRequestMsg{ID: id, Request: request, Reply: reply})
	var result approval.Result
	select {
	case result = <-reply:
	case <-ctx.Done():
		program.Send(approvalCanceledMsg{ID: id})
		return approval.Result{}, ctx.Err()
	}
	if result.Decision == approval.ApprovedAll {
		h.mu.Lock()
		h.approvedAll = true
		h.mu.Unlock()
	}
	return result, nil
}
