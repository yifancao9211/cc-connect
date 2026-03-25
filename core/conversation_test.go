package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConversationStore_GetOrCreate(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("feishu:oc_chat:ou_user")

	if conv == nil {
		t.Fatal("GetOrCreate returned nil")
	}
	if conv.Key != "feishu:oc_chat:ou_user" {
		t.Errorf("Key = %q, want feishu:oc_chat:ou_user", conv.Key)
	}

	conv2 := store.GetOrCreate("feishu:oc_chat:ou_user")
	if conv2 != conv {
		t.Error("GetOrCreate should return same instance for same key")
	}
}

func TestConversationStore_Get_NotFound(t *testing.T) {
	store := NewConversationStore("")
	conv := store.Get("nonexistent")
	if conv != nil {
		t.Error("Get should return nil for nonexistent key")
	}
}

func TestConversationContext_TryLock(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")

	if !conv.TryLock() {
		t.Fatal("first TryLock should succeed")
	}
	if conv.TryLock() {
		t.Fatal("second TryLock should fail (already busy)")
	}
	conv.Unlock()
	if !conv.TryLock() {
		t.Fatal("TryLock after Unlock should succeed")
	}
	conv.Unlock()
}

func TestConversationContext_History(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")

	conv.AddHistory("user", "hello")
	conv.AddHistory("assistant", "hi there")
	conv.AddHistory("user", "how are you")

	entries := conv.GetHistory(2)
	if len(entries) != 2 {
		t.Fatalf("GetHistory(2) returned %d entries, want 2", len(entries))
	}
	if entries[0].Role != "assistant" {
		t.Errorf("entries[0].Role = %q, want assistant", entries[0].Role)
	}
	if entries[1].Content != "how are you" {
		t.Errorf("entries[1].Content = %q, want 'how are you'", entries[1].Content)
	}
}

func TestConversationContext_HistoryLimit(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")

	for i := 0; i < 250; i++ {
		conv.AddHistory("user", "msg")
	}

	entries := conv.GetHistory(0)
	if len(entries) > maxHistoryEntries {
		t.Errorf("history length = %d, should be capped at %d", len(entries), maxHistoryEntries)
	}
}

func TestConversationContext_ClearHistory(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")
	conv.AddHistory("user", "hello")
	conv.ClearHistory()

	if entries := conv.GetHistory(0); len(entries) != 0 {
		t.Errorf("after ClearHistory, got %d entries", len(entries))
	}
}

func TestConversationContext_LastUserMessage(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")

	if msg := conv.LastUserMessage(); msg != "" {
		t.Errorf("empty history should return empty, got %q", msg)
	}

	conv.AddHistory("user", "first")
	conv.AddHistory("assistant", "response")
	conv.AddHistory("user", "second")

	if msg := conv.LastUserMessage(); msg != "second" {
		t.Errorf("LastUserMessage = %q, want 'second'", msg)
	}
}

func TestConversationContext_ApprovalPhase_Default(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")

	if conv.ApprovalPhase != PhasePlanning {
		t.Errorf("default ApprovalPhase = %v, want PhasePlanning", conv.ApprovalPhase)
	}
}

func TestConversationStore_List(t *testing.T) {
	store := NewConversationStore("")
	store.GetOrCreate("feishu:chat1:user1")
	store.GetOrCreate("feishu:chat2:user2")
	store.GetOrCreate("telegram:chat3:user3")

	all := store.List()
	if len(all) != 3 {
		t.Errorf("List() returned %d, want 3", len(all))
	}
}

func TestConversationStore_Delete(t *testing.T) {
	store := NewConversationStore("")
	store.GetOrCreate("test:user1")
	store.Delete("test:user1")

	if conv := store.Get("test:user1"); conv != nil {
		t.Error("after Delete, Get should return nil")
	}
}

func TestConversationStore_Persistence(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "conversations.json")

	store1 := NewConversationStore(storePath)
	conv := store1.GetOrCreate("feishu:chat:user")
	conv.AgentSessionID = "agent-session-abc"
	conv.Name = "My Session"
	conv.AddHistory("user", "hello world")
	conv.ApprovalPhase = PhasePending
	conv.PlanText = "refactor plan"
	store1.Save()

	store2 := NewConversationStore(storePath)
	conv2 := store2.Get("feishu:chat:user")
	if conv2 == nil {
		t.Fatal("loaded store should have the conversation")
	}
	if conv2.AgentSessionID != "agent-session-abc" {
		t.Errorf("AgentSessionID = %q, want agent-session-abc", conv2.AgentSessionID)
	}
	if conv2.Name != "My Session" {
		t.Errorf("Name = %q, want 'My Session'", conv2.Name)
	}
	entries := conv2.GetHistory(0)
	if len(entries) != 1 || entries[0].Content != "hello world" {
		t.Errorf("History not persisted correctly: %v", entries)
	}
	if conv2.ApprovalPhase != PhasePending {
		t.Errorf("ApprovalPhase = %v, want PhasePending", conv2.ApprovalPhase)
	}
	if conv2.PlanText != "refactor plan" {
		t.Errorf("PlanText = %q, want 'refactor plan'", conv2.PlanText)
	}
}

func TestConversationStore_Persistence_Empty(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "conversations.json")

	store := NewConversationStore(storePath)
	all := store.List()
	if len(all) != 0 {
		t.Errorf("new store should be empty, got %d", len(all))
	}
}

func TestConversationStore_Persistence_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "conversations.json")
	os.WriteFile(storePath, []byte("{invalid json"), 0644)

	store := NewConversationStore(storePath)
	all := store.List()
	if len(all) != 0 {
		t.Errorf("corrupt file should result in empty store, got %d", len(all))
	}
}

func TestConversationContext_AgentName(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")

	if conv.AgentName != "" {
		t.Errorf("default AgentName should be empty, got %q", conv.AgentName)
	}
	conv.AgentName = "cursor"
	if conv.AgentName != "cursor" {
		t.Errorf("AgentName = %q, want cursor", conv.AgentName)
	}
}

// ── Phase 1.1: New fields ─────────────────────────────────────

func TestConversationContext_RuntimeFields(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")

	if conv.ApproveAll {
		t.Error("default ApproveAll should be false")
	}
	if conv.FromVoice {
		t.Error("default FromVoice should be false")
	}
	if conv.WorkspaceDir != "" {
		t.Errorf("default WorkspaceDir should be empty, got %q", conv.WorkspaceDir)
	}

	conv.ApproveAll = true
	conv.FromVoice = true
	conv.WorkspaceDir = "/tmp/workspace"

	if !conv.ApproveAll {
		t.Error("ApproveAll should be true after set")
	}
	if !conv.FromVoice {
		t.Error("FromVoice should be true after set")
	}
	if conv.WorkspaceDir != "/tmp/workspace" {
		t.Errorf("WorkspaceDir = %q, want /tmp/workspace", conv.WorkspaceDir)
	}
}

func TestConversationContext_RuntimeFieldsNotPersisted(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "conversations.json")

	store1 := NewConversationStore(storePath)
	conv := store1.GetOrCreate("test:user1")
	conv.ApproveAll = true
	conv.FromVoice = true
	conv.WorkspaceDir = "/tmp/ws"
	conv.Quiet = true
	store1.Save()

	store2 := NewConversationStore(storePath)
	conv2 := store2.Get("test:user1")
	if conv2 == nil {
		t.Fatal("loaded store should have the conversation")
	}
	if conv2.ApproveAll {
		t.Error("ApproveAll should not be persisted")
	}
	if conv2.FromVoice {
		t.Error("FromVoice should not be persisted")
	}
	if conv2.WorkspaceDir != "" {
		t.Error("WorkspaceDir should not be persisted")
	}
	if conv2.Quiet {
		t.Error("Quiet should not be persisted")
	}
}

// ── Phase 1.1: SetAgentInfo / CompareAndSetAgentSessionID ─────

func TestConversationContext_SetAgentInfo(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")

	conv.SetAgentInfo("session-123", "My Project")
	if conv.AgentSessionID != "session-123" {
		t.Errorf("AgentSessionID = %q, want session-123", conv.AgentSessionID)
	}
	if conv.Name != "My Project" {
		t.Errorf("Name = %q, want 'My Project'", conv.Name)
	}
}

func TestConversationContext_SetAgentSessionID(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")

	conv.SetAgentSessionID("abc")
	if conv.AgentSessionID != "abc" {
		t.Errorf("AgentSessionID = %q, want abc", conv.AgentSessionID)
	}
	conv.SetAgentSessionID("def")
	if conv.AgentSessionID != "def" {
		t.Errorf("AgentSessionID = %q, want def", conv.AgentSessionID)
	}
}

func TestConversationContext_CompareAndSetAgentSessionID(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")

	if !conv.CompareAndSetAgentSessionID("first") {
		t.Fatal("CAS on empty should succeed")
	}
	if conv.AgentSessionID != "first" {
		t.Errorf("AgentSessionID = %q, want first", conv.AgentSessionID)
	}

	if conv.CompareAndSetAgentSessionID("second") {
		t.Fatal("CAS on non-empty should fail")
	}
	if conv.AgentSessionID != "first" {
		t.Errorf("AgentSessionID should remain 'first', got %q", conv.AgentSessionID)
	}
}

// ── Phase 1.1: Approval state machine on ConversationContext ──

func TestConversationContext_SubmitPlan(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")

	if !conv.SubmitPlan("refactor modules", "1. analyze\n2. refactor") {
		t.Fatal("SubmitPlan from Planning should succeed")
	}
	if conv.ApprovalPhase != PhasePending {
		t.Errorf("phase = %v, want PhasePending", conv.ApprovalPhase)
	}
	if conv.PlanText != "1. analyze\n2. refactor" {
		t.Errorf("PlanText = %q", conv.PlanText)
	}
	if conv.UserMessage != "refactor modules" {
		t.Errorf("UserMessage = %q", conv.UserMessage)
	}

	if conv.SubmitPlan("another", "plan") {
		t.Fatal("SubmitPlan from Pending should fail")
	}
}

func TestConversationContext_Approve(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")

	if conv.Approve("admin1", "looks good") {
		t.Fatal("Approve from Planning should fail")
	}

	conv.SubmitPlan("task", "plan")
	if !conv.Approve("admin1", "looks good") {
		t.Fatal("Approve from Pending should succeed")
	}
	if conv.ApprovalPhase != PhaseExecuting {
		t.Errorf("phase = %v, want PhaseExecuting", conv.ApprovalPhase)
	}
	if conv.ReviewerID != "admin1" {
		t.Errorf("ReviewerID = %q, want admin1", conv.ReviewerID)
	}
	if conv.ReviewerNote != "looks good" {
		t.Errorf("ReviewerNote = %q, want 'looks good'", conv.ReviewerNote)
	}
}

func TestConversationContext_Reject(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")

	conv.SubmitPlan("task", "plan")
	if !conv.Reject("admin1", "too risky") {
		t.Fatal("Reject from Pending should succeed")
	}
	if conv.ApprovalPhase != PhasePlanning {
		t.Errorf("phase = %v, want PhasePlanning", conv.ApprovalPhase)
	}
	if conv.PlanText != "" {
		t.Errorf("PlanText should be cleared after reject, got %q", conv.PlanText)
	}
	if conv.ReviewerID != "admin1" {
		t.Errorf("ReviewerID = %q", conv.ReviewerID)
	}
	if conv.ReviewerNote != "too risky" {
		t.Errorf("ReviewerNote = %q", conv.ReviewerNote)
	}
}

func TestConversationContext_CompleteExecution(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")

	conv.SubmitPlan("task", "plan")
	conv.Approve("admin1", "")
	conv.ApproveAll = true

	conv.CompleteExecution()
	if conv.ApprovalPhase != PhasePlanning {
		t.Errorf("phase = %v, want PhasePlanning", conv.ApprovalPhase)
	}
	if conv.PlanText != "" {
		t.Error("PlanText should be cleared")
	}
	if conv.ReviewerID != "" {
		t.Error("ReviewerID should be cleared")
	}
	if conv.ReviewerNote != "" {
		t.Error("ReviewerNote should be cleared")
	}
	if conv.ApproveAll {
		t.Error("ApproveAll should be cleared on complete")
	}
}

func TestConversationContext_ApprovalStateMachine_FullCycle(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")

	// Planning → Pending → Executing → Planning
	if conv.ApprovalPhase != PhasePlanning {
		t.Fatalf("initial phase = %v", conv.ApprovalPhase)
	}
	conv.SubmitPlan("msg", "plan")
	if conv.ApprovalPhase != PhasePending {
		t.Fatalf("after submit = %v", conv.ApprovalPhase)
	}
	conv.Approve("admin", "ok")
	if conv.ApprovalPhase != PhaseExecuting {
		t.Fatalf("after approve = %v", conv.ApprovalPhase)
	}
	conv.CompleteExecution()
	if conv.ApprovalPhase != PhasePlanning {
		t.Fatalf("after complete = %v", conv.ApprovalPhase)
	}

	// Second cycle: Planning → Pending → Reject → Planning
	conv.SubmitPlan("msg2", "plan2")
	conv.Reject("admin", "no")
	if conv.ApprovalPhase != PhasePlanning {
		t.Fatalf("after reject = %v", conv.ApprovalPhase)
	}
}

// ── Phase 1.1: ClearRuntime ───────────────────────────────────

func TestConversationContext_ClearRuntime(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")

	conv.AgentSession = nil // just setting to check it gets cleared
	conv.ReplyPlatform = nil
	conv.ReplyCtx = "some ctx"
	conv.Quiet = true
	conv.ApproveAll = true
	conv.FromVoice = true
	conv.DeleteMode = &deleteModeState{}
	conv.PendingPerm = &pendingPermission{RequestID: "test"}

	conv.ClearRuntime()

	if conv.AgentSession != nil {
		t.Error("AgentSession should be nil")
	}
	if conv.ReplyPlatform != nil {
		t.Error("ReplyPlatform should be nil")
	}
	if conv.ReplyCtx != nil {
		t.Error("ReplyCtx should be nil")
	}
	if conv.Quiet {
		t.Error("Quiet should be false")
	}
	if conv.ApproveAll {
		t.Error("ApproveAll should be false")
	}
	if conv.FromVoice {
		t.Error("FromVoice should be false")
	}
	if conv.DeleteMode != nil {
		t.Error("DeleteMode should be nil")
	}
	if conv.PendingPerm != nil {
		t.Error("PendingPerm should be nil")
	}

	// Persistent fields should NOT be cleared
	if conv.Key != "test:user1" {
		t.Error("Key should be preserved")
	}
}

// ── Phase 1.1: Multi-session support ──────────────────────────

func TestConversationContext_NewSession(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")
	conv.AgentSessionID = "agent-1"
	conv.Name = "session one"
	conv.AddHistory("user", "hello")

	slotID := conv.NewSession("session two")
	if slotID == "" {
		t.Fatal("NewSession should return a non-empty slot ID")
	}

	// Current state should be cleared
	if conv.AgentSessionID != "" {
		t.Errorf("AgentSessionID should be empty after NewSession, got %q", conv.AgentSessionID)
	}
	if conv.Name != "session two" {
		t.Errorf("Name = %q, want 'session two'", conv.Name)
	}
	if h := conv.GetHistory(0); len(h) != 0 {
		t.Errorf("History should be empty after NewSession, got %d", len(h))
	}

	// Old session should be saved
	if conv.SavedSessions == nil {
		t.Fatal("SavedSessions should not be nil")
	}
	saved, ok := conv.SavedSessions[slotID]
	if !ok {
		t.Fatal("old session not found in SavedSessions")
	}
	if saved.AgentSessionID != "agent-1" {
		t.Errorf("saved AgentSessionID = %q, want agent-1", saved.AgentSessionID)
	}
	if saved.Name != "session one" {
		t.Errorf("saved Name = %q, want 'session one'", saved.Name)
	}
}

func TestConversationContext_SwitchSession(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")
	conv.AgentSessionID = "agent-1"
	conv.Name = "first"
	conv.AddHistory("user", "msg1")

	savedID := conv.NewSession("second")
	conv.AgentSessionID = "agent-2"
	conv.AddHistory("user", "msg2")

	// Switch back to the first session
	if err := conv.SwitchSession(savedID); err != nil {
		t.Fatalf("SwitchSession failed: %v", err)
	}
	if conv.AgentSessionID != "agent-1" {
		t.Errorf("after switch, AgentSessionID = %q, want agent-1", conv.AgentSessionID)
	}
	if conv.Name != "first" {
		t.Errorf("after switch, Name = %q, want first", conv.Name)
	}
	h := conv.GetHistory(0)
	if len(h) != 1 || h[0].Content != "msg1" {
		t.Errorf("after switch, history wrong: %v", h)
	}

	// The "second" session should now be saved
	found := false
	for _, s := range conv.SavedSessions {
		if s.AgentSessionID == "agent-2" {
			found = true
			if s.Name != "second" {
				t.Errorf("saved Name = %q, want second", s.Name)
			}
		}
	}
	if !found {
		t.Error("second session not found in SavedSessions after switch")
	}
}

func TestConversationContext_SwitchSession_NotFound(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")

	if err := conv.SwitchSession("nonexistent"); err == nil {
		t.Error("SwitchSession to nonexistent should fail")
	}
}

func TestConversationContext_ListSessions(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")
	conv.AgentSessionID = "agent-1"
	conv.Name = "first"

	conv.NewSession("second")
	conv.AgentSessionID = "agent-2"

	conv.NewSession("third")
	conv.AgentSessionID = "agent-3"

	sessions := conv.ListSessions()
	if len(sessions) != 3 {
		t.Fatalf("ListSessions returned %d, want 3", len(sessions))
	}

	names := make(map[string]bool)
	for _, s := range sessions {
		names[s.Name] = true
	}
	for _, want := range []string{"first", "second", "third"} {
		if !names[want] {
			t.Errorf("missing session %q in list", want)
		}
	}
}

func TestConversationContext_SwitchSession_ByName(t *testing.T) {
	store := NewConversationStore("")
	conv := store.GetOrCreate("test:user1")
	conv.AgentSessionID = "agent-1"
	conv.Name = "first"

	conv.NewSession("second")
	conv.AgentSessionID = "agent-2"

	// Switch by name
	if err := conv.SwitchSession("first"); err != nil {
		t.Fatalf("SwitchSession by name failed: %v", err)
	}
	if conv.AgentSessionID != "agent-1" {
		t.Errorf("AgentSessionID = %q, want agent-1", conv.AgentSessionID)
	}
}

func TestConversationContext_MultiSession_Persistence(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "conversations.json")

	store1 := NewConversationStore(storePath)
	conv := store1.GetOrCreate("test:user1")
	conv.AgentSessionID = "agent-1"
	conv.Name = "first"
	conv.AddHistory("user", "hello")
	conv.NewSession("second")
	conv.AgentSessionID = "agent-2"
	store1.Save()

	store2 := NewConversationStore(storePath)
	conv2 := store2.Get("test:user1")
	if conv2 == nil {
		t.Fatal("loaded store should have the conversation")
	}

	sessions := conv2.ListSessions()
	if len(sessions) != 2 {
		t.Fatalf("ListSessions after load = %d, want 2", len(sessions))
	}
}
