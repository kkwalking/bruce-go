package tui

import (
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"bruce-go/internal/approval"
)

type approvalRequestMsg struct {
	Request approval.Request
	Reply   chan approval.Result
}

type tuiApprovalHandler struct {
	mu          sync.RWMutex
	enabled     bool
	approvedAll bool
	program     *tea.Program
}

func newTUIApprovalHandler() *tuiApprovalHandler {
	return &tuiApprovalHandler{enabled: false}
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

func (h *tuiApprovalHandler) Request(request approval.Request) approval.Result {
	h.mu.RLock()
	enabled := h.enabled
	approvedAll := h.approvedAll
	program := h.program
	h.mu.RUnlock()
	if !enabled || approvedAll {
		return approval.Approve()
	}
	if program == nil {
		return approval.Approve()
	}
	reply := make(chan approval.Result, 1)
	program.Send(approvalRequestMsg{Request: request, Reply: reply})
	result := <-reply
	if result.Decision == approval.ApprovedAll {
		h.mu.Lock()
		h.approvedAll = true
		h.mu.Unlock()
	}
	return result
}
