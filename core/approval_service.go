package core

import (
	"fmt"
	"log/slog"
	"time"
)

// ApprovalService provides scheme-level approval orchestration on top of
// ConversationContext's state machine. It is the single entry point for
// submit-plan / approve / reject / trust-session actions.
type ApprovalService struct {
	conversations *ConversationStore
	notifyFunc    func(sessionKey, message string) error
	timeout       time.Duration
}

func NewApprovalService(conversations *ConversationStore, timeout time.Duration) *ApprovalService {
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	return &ApprovalService{
		conversations: conversations,
		timeout:       timeout,
	}
}

func (a *ApprovalService) SetNotifyFunc(f func(sessionKey, message string) error) {
	a.notifyFunc = f
}

// SubmitPlan transitions Planning → Pending and records the plan.
func (a *ApprovalService) SubmitPlan(sessionKey, userMsg, plan string) error {
	conv := a.conversations.GetOrCreate(sessionKey)
	if !conv.SubmitPlan(userMsg, plan) {
		return fmt.Errorf("cannot submit plan: current phase is %s", conv.ApprovalPhase)
	}
	return nil
}

// Approve transitions Pending → Executing.
func (a *ApprovalService) Approve(sessionKey, reviewer, note string) error {
	conv := a.conversations.GetOrCreate(sessionKey)
	if !conv.Approve(reviewer, note) {
		return fmt.Errorf("cannot approve: current phase is %s", conv.ApprovalPhase)
	}
	return nil
}

// TrustSession approves and sets ApproveAll so subsequent tool calls
// in this session skip per-call approval.
func (a *ApprovalService) TrustSession(sessionKey, reviewer, note string) error {
	conv := a.conversations.GetOrCreate(sessionKey)
	if !conv.Approve(reviewer, note) {
		return fmt.Errorf("cannot trust session: current phase is %s", conv.ApprovalPhase)
	}
	conv.ApproveAll = true
	return nil
}

// Reject transitions Pending → Planning and stores reviewer feedback.
func (a *ApprovalService) Reject(sessionKey, reviewer, note string) error {
	conv := a.conversations.GetOrCreate(sessionKey)
	if !conv.Reject(reviewer, note) {
		return fmt.Errorf("cannot reject: current phase is %s", conv.ApprovalPhase)
	}
	return nil
}

// CheckTimeouts resets conversations that have been in Pending phase
// longer than the configured timeout back to Planning.
func (a *ApprovalService) CheckTimeouts() {
	cutoff := time.Now().Add(-a.timeout)
	for _, conv := range a.conversations.List() {
		conv.mu.Lock()
		if conv.ApprovalPhase == PhasePending && conv.UpdatedAt.Before(cutoff) {
			conv.ApprovalPhase = PhasePlanning
			conv.PlanText = ""
			conv.ReviewerID = ""
			conv.ReviewerNote = ""
			conv.UpdatedAt = time.Now()
			slog.Info("approval: timeout, reset to planning", "key", conv.Key)
		}
		conv.mu.Unlock()
	}
}
