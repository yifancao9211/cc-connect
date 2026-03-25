package core

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestApprovalService(t *testing.T) (*ApprovalService, *ConversationStore) {
	t.Helper()
	store := NewConversationStore("")
	svc := NewApprovalService(store, 5*time.Minute)
	return svc, store
}

func TestApprovalService_SubmitPlan(t *testing.T) {
	svc, store := newTestApprovalService(t)

	var notified []string
	svc.SetNotifyFunc(func(sessionKey, message string) error {
		notified = append(notified, sessionKey+"|"+message)
		return nil
	})

	key := "feishu:chat1:user1"
	store.GetOrCreate(key) // ensure conv exists in Planning

	if err := svc.SubmitPlan(key, "refactor the auth module", "1. extract interface\n2. add tests"); err != nil {
		t.Fatalf("SubmitPlan failed: %v", err)
	}

	conv := store.Get(key)
	if conv.ApprovalPhase != PhasePending {
		t.Errorf("phase = %v, want PhasePending", conv.ApprovalPhase)
	}
	if conv.PlanText != "1. extract interface\n2. add tests" {
		t.Errorf("PlanText = %q", conv.PlanText)
	}
	if conv.UserMessage != "refactor the auth module" {
		t.Errorf("UserMessage = %q", conv.UserMessage)
	}
}

func TestApprovalService_SubmitPlan_WrongPhase(t *testing.T) {
	svc, store := newTestApprovalService(t)

	key := "feishu:chat1:user1"
	conv := store.GetOrCreate(key)

	// Move to Pending first
	conv.SubmitPlan("msg", "plan")

	// Attempting to submit again from Pending should fail
	if err := svc.SubmitPlan(key, "msg2", "plan2"); err == nil {
		t.Fatal("SubmitPlan from Pending should return error")
	}
}

func TestApprovalService_Approve(t *testing.T) {
	svc, store := newTestApprovalService(t)

	key := "feishu:chat1:user1"
	store.GetOrCreate(key)
	svc.SubmitPlan(key, "task", "plan")

	if err := svc.Approve(key, "admin1", "looks good"); err != nil {
		t.Fatalf("Approve failed: %v", err)
	}

	conv := store.Get(key)
	if conv.ApprovalPhase != PhaseExecuting {
		t.Errorf("phase = %v, want PhaseExecuting", conv.ApprovalPhase)
	}
	if conv.ReviewerID != "admin1" {
		t.Errorf("ReviewerID = %q, want admin1", conv.ReviewerID)
	}
	if conv.ReviewerNote != "looks good" {
		t.Errorf("ReviewerNote = %q", conv.ReviewerNote)
	}
}

func TestApprovalService_Approve_WrongPhase(t *testing.T) {
	svc, store := newTestApprovalService(t)

	key := "feishu:chat1:user1"
	store.GetOrCreate(key) // Planning phase

	if err := svc.Approve(key, "admin", "ok"); err == nil {
		t.Fatal("Approve from Planning should return error")
	}
}

func TestApprovalService_TrustSession(t *testing.T) {
	svc, store := newTestApprovalService(t)

	key := "feishu:chat1:user1"
	store.GetOrCreate(key)
	svc.SubmitPlan(key, "task", "plan")

	if err := svc.TrustSession(key, "admin1", "trusted"); err != nil {
		t.Fatalf("TrustSession failed: %v", err)
	}

	conv := store.Get(key)
	if conv.ApprovalPhase != PhaseExecuting {
		t.Errorf("phase = %v, want PhaseExecuting", conv.ApprovalPhase)
	}
	if !conv.ApproveAll {
		t.Error("ApproveAll should be true after TrustSession")
	}
	if conv.ReviewerID != "admin1" {
		t.Errorf("ReviewerID = %q, want admin1", conv.ReviewerID)
	}
}

func TestApprovalService_TrustSession_WrongPhase(t *testing.T) {
	svc, store := newTestApprovalService(t)

	key := "feishu:chat1:user1"
	store.GetOrCreate(key)

	if err := svc.TrustSession(key, "admin", "trust"); err == nil {
		t.Fatal("TrustSession from Planning should return error")
	}
}

func TestApprovalService_Reject(t *testing.T) {
	svc, store := newTestApprovalService(t)

	key := "feishu:chat1:user1"
	store.GetOrCreate(key)
	svc.SubmitPlan(key, "deploy to prod", "1. build\n2. deploy")

	if err := svc.Reject(key, "admin1", "too risky"); err != nil {
		t.Fatalf("Reject failed: %v", err)
	}

	conv := store.Get(key)
	if conv.ApprovalPhase != PhasePlanning {
		t.Errorf("phase = %v, want PhasePlanning", conv.ApprovalPhase)
	}
	if conv.PlanText != "" {
		t.Errorf("PlanText should be cleared, got %q", conv.PlanText)
	}
	if conv.ReviewerID != "admin1" {
		t.Errorf("ReviewerID = %q, want admin1", conv.ReviewerID)
	}
	if conv.ReviewerNote != "too risky" {
		t.Errorf("ReviewerNote = %q, want 'too risky'", conv.ReviewerNote)
	}
}

func TestApprovalService_Reject_WrongPhase(t *testing.T) {
	svc, store := newTestApprovalService(t)

	key := "feishu:chat1:user1"
	store.GetOrCreate(key)

	if err := svc.Reject(key, "admin", "no"); err == nil {
		t.Fatal("Reject from Planning should return error")
	}
}

func TestApprovalService_CheckTimeouts(t *testing.T) {
	store := NewConversationStore("")
	svc := NewApprovalService(store, 100*time.Millisecond)

	key := "feishu:chat1:user1"
	store.GetOrCreate(key)
	svc.SubmitPlan(key, "task", "plan")

	// Not timed out yet
	svc.CheckTimeouts()
	conv := store.Get(key)
	if conv.ApprovalPhase != PhasePending {
		t.Errorf("before timeout: phase = %v, want PhasePending", conv.ApprovalPhase)
	}

	// Wait for timeout
	time.Sleep(150 * time.Millisecond)
	svc.CheckTimeouts()

	conv = store.Get(key)
	if conv.ApprovalPhase != PhasePlanning {
		t.Errorf("after timeout: phase = %v, want PhasePlanning", conv.ApprovalPhase)
	}
	if conv.PlanText != "" {
		t.Errorf("PlanText should be cleared after timeout, got %q", conv.PlanText)
	}
	if conv.ReviewerID != "" {
		t.Errorf("ReviewerID should be cleared after timeout, got %q", conv.ReviewerID)
	}
}

func TestApprovalService_CheckTimeouts_OnlyPending(t *testing.T) {
	store := NewConversationStore("")
	svc := NewApprovalService(store, 100*time.Millisecond)

	// Planning phase should not be affected
	planningKey := "feishu:chat1:user1"
	store.GetOrCreate(planningKey)

	// Executing phase should not be affected
	execKey := "feishu:chat2:user2"
	store.GetOrCreate(execKey)
	svc.SubmitPlan(execKey, "task", "plan")
	svc.Approve(execKey, "admin", "ok")

	time.Sleep(150 * time.Millisecond)
	svc.CheckTimeouts()

	if store.Get(planningKey).ApprovalPhase != PhasePlanning {
		t.Error("Planning conv should not be affected by timeout")
	}
	if store.Get(execKey).ApprovalPhase != PhaseExecuting {
		t.Error("Executing conv should not be affected by timeout")
	}
}

func TestApprovalService_Persistence(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "conversations.json")

	store1 := NewConversationStore(storePath)
	svc1 := NewApprovalService(store1, 30*time.Minute)

	key := "feishu:chat1:user1"
	store1.GetOrCreate(key)
	svc1.SubmitPlan(key, "refactor DB layer", "1. migrate schema\n2. update queries")
	store1.Save()

	// Load in a new store — approval state should survive
	store2 := NewConversationStore(storePath)
	conv2 := store2.Get(key)
	if conv2 == nil {
		t.Fatal("conversation should survive persistence")
	}
	if conv2.ApprovalPhase != PhasePending {
		t.Errorf("phase = %v, want PhasePending", conv2.ApprovalPhase)
	}
	if conv2.PlanText != "1. migrate schema\n2. update queries" {
		t.Errorf("PlanText = %q", conv2.PlanText)
	}
	if conv2.UserMessage != "refactor DB layer" {
		t.Errorf("UserMessage = %q", conv2.UserMessage)
	}

	// Should be able to approve on the reloaded store
	svc2 := NewApprovalService(store2, 30*time.Minute)
	if err := svc2.Approve(key, "admin", "go ahead"); err != nil {
		t.Fatalf("Approve after reload failed: %v", err)
	}
	if conv2.ApprovalPhase != PhaseExecuting {
		t.Errorf("after approve: phase = %v, want PhaseExecuting", conv2.ApprovalPhase)
	}
}

func TestApprovalService_DefaultTimeout(t *testing.T) {
	store := NewConversationStore("")
	svc := NewApprovalService(store, 0)

	if svc.timeout != 30*time.Minute {
		t.Errorf("default timeout = %v, want 30m", svc.timeout)
	}

	svc2 := NewApprovalService(store, -5*time.Second)
	if svc2.timeout != 30*time.Minute {
		t.Errorf("negative timeout should default to 30m, got %v", svc2.timeout)
	}
}

func TestApprovalService_ConcurrentAccess(t *testing.T) {
	svc, store := newTestApprovalService(t)

	key := "feishu:chat1:user1"
	store.GetOrCreate(key)

	var wg sync.WaitGroup
	errs := make(chan error, 10)

	// Multiple goroutines try to submit simultaneously — only one should succeed
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.SubmitPlan(key, "msg", "plan"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	errCount := 0
	for range errs {
		errCount++
	}
	// 9 of 10 should fail (only 1 can transition Planning → Pending)
	if errCount != 9 {
		t.Errorf("expected 9 errors from concurrent submit, got %d", errCount)
	}
}
