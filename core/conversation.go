package core

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

const maxHistoryEntries = 200

// ConversationContext is the unified state for a single conversation.
// It merges the previously separate Session, interactiveState, and ApprovalState
// into one structure with one key, one lock, and one lifecycle.
type ConversationContext struct {
	mu sync.Mutex `json:"-"`

	// Identity
	Key       string `json:"key"`
	Platform  string `json:"platform,omitempty"`
	OwnerID   string `json:"owner_id,omitempty"`
	AgentName string `json:"agent_name,omitempty"`

	// Persistent state
	AgentSessionID string         `json:"agent_session_id,omitempty"`
	History        []HistoryEntry `json:"history,omitempty"`
	Name           string         `json:"name,omitempty"`

	// Approval state (persistent)
	ApprovalPhase ApprovalPhase `json:"approval_phase,omitempty"`
	PlanText      string        `json:"plan_text,omitempty"`
	ReviewerID    string        `json:"reviewer_id,omitempty"`
	ReviewerNote  string        `json:"reviewer_note,omitempty"`

	// Approval state (persistent) — user message that triggered the plan
	UserMessage string `json:"user_message,omitempty"`

	// Multi-session support (persistent)
	SavedSessions map[string]*SessionSlot `json:"saved_sessions,omitempty"`
	sessionCounter int64

	// Runtime state (not persisted)
	AgentSession  AgentSession `json:"-"`
	ReplyPlatform Platform     `json:"-"`
	ReplyCtx      any          `json:"-"`
	busy          bool
	Quiet         bool `json:"-"`
	ApproveAll    bool `json:"-"`
	FromVoice     bool `json:"-"`
	WorkspaceDir  string `json:"-"`

	// UI state (not persisted)
	DeleteMode  *deleteModeState   `json:"-"`
	PendingPerm *pendingPermission `json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SessionSlot holds the state of an archived (non-active) session.
type SessionSlot struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	AgentSessionID string         `json:"agent_session_id"`
	History        []HistoryEntry `json:"history"`
	CreatedAt      time.Time      `json:"created_at"`
}

func (c *ConversationContext) TryLock() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy {
		return false
	}
	c.busy = true
	return true
}

func (c *ConversationContext) Unlock() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.busy = false
	c.UpdatedAt = time.Now()
}

func (c *ConversationContext) AddHistory(role, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.History = append(c.History, HistoryEntry{
		Role:      role,
		Content:   content,
		Timestamp: time.Now(),
	})
	if len(c.History) > maxHistoryEntries {
		c.History = c.History[len(c.History)-maxHistoryEntries:]
	}
}

func (c *ConversationContext) ClearHistory() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.History = nil
}

func (c *ConversationContext) GetHistory(n int) []HistoryEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := len(c.History)
	if n <= 0 || n > total {
		n = total
	}
	out := make([]HistoryEntry, n)
	copy(out, c.History[total-n:])
	return out
}

func (c *ConversationContext) LastUserMessage() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.History) - 1; i >= 0; i-- {
		if c.History[i].Role == "user" {
			return c.History[i].Content
		}
	}
	return ""
}

// ── Agent session helpers ─────────────────────────────────────

// SetAgentInfo atomically sets the agent session ID and name.
func (c *ConversationContext) SetAgentInfo(agentSessionID, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.AgentSessionID = agentSessionID
	c.Name = name
}

// SetAgentSessionID atomically sets the agent session ID.
func (c *ConversationContext) SetAgentSessionID(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.AgentSessionID = id
}

// CompareAndSetAgentSessionID sets the agent session ID only if it is currently empty.
func (c *ConversationContext) CompareAndSetAgentSessionID(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.AgentSessionID != "" {
		return false
	}
	c.AgentSessionID = id
	return true
}

// ── Approval state machine ───────────────────────────────────

// SubmitPlan transitions Planning → Pending.
func (c *ConversationContext) SubmitPlan(userMsg, plan string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ApprovalPhase != PhasePlanning {
		slog.Warn("approval: submit-plan in wrong phase", "key", c.Key, "phase", c.ApprovalPhase)
		return false
	}
	c.ApprovalPhase = PhasePending
	c.UserMessage = userMsg
	c.PlanText = plan
	c.UpdatedAt = time.Now()
	slog.Info("approval: plan submitted", "key", c.Key)
	return true
}

// Approve transitions Pending → Executing.
func (c *ConversationContext) Approve(reviewer, note string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ApprovalPhase != PhasePending {
		slog.Warn("approval: approve in wrong phase", "key", c.Key, "phase", c.ApprovalPhase)
		return false
	}
	c.ApprovalPhase = PhaseExecuting
	c.ReviewerID = reviewer
	c.ReviewerNote = note
	c.UpdatedAt = time.Now()
	slog.Info("approval: approved", "key", c.Key, "reviewer", reviewer)
	return true
}

// Reject transitions Pending → Planning.
func (c *ConversationContext) Reject(reviewer, note string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ApprovalPhase != PhasePending {
		return false
	}
	c.ApprovalPhase = PhasePlanning
	c.PlanText = ""
	c.ReviewerID = reviewer
	c.ReviewerNote = note
	c.UpdatedAt = time.Now()
	slog.Info("approval: rejected", "key", c.Key, "reviewer", reviewer)
	return true
}

// CompleteExecution transitions Executing → Planning and resets approval state.
func (c *ConversationContext) CompleteExecution() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ApprovalPhase != PhaseExecuting {
		return
	}
	c.ApprovalPhase = PhasePlanning
	c.PlanText = ""
	c.ReviewerID = ""
	c.ReviewerNote = ""
	c.ApproveAll = false
	c.UpdatedAt = time.Now()
	slog.Info("approval: execution completed, reset to planning", "key", c.Key)
}

// ── Runtime lifecycle ─────────────────────────────────────────

// ClearRuntime resets all runtime (non-persisted) state.
// Used when an agent session ends or needs cleanup.
func (c *ConversationContext) ClearRuntime() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.AgentSession = nil
	c.ReplyPlatform = nil
	c.ReplyCtx = nil
	c.Quiet = false
	c.ApproveAll = false
	c.FromVoice = false
	c.DeleteMode = nil
	c.PendingPerm = nil
}

// ── Multi-session support ─────────────────────────────────────

func (c *ConversationContext) nextSlotID() string {
	c.sessionCounter++
	return fmt.Sprintf("s%d", c.sessionCounter)
}

// NewSession archives the current session state and starts a fresh one.
// Returns the slot ID of the archived session.
func (c *ConversationContext) NewSession(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	slotID := c.nextSlotID()

	if c.SavedSessions == nil {
		c.SavedSessions = make(map[string]*SessionSlot)
	}
	c.SavedSessions[slotID] = &SessionSlot{
		ID:             slotID,
		Name:           c.Name,
		AgentSessionID: c.AgentSessionID,
		History:        append([]HistoryEntry(nil), c.History...),
		CreatedAt:      c.CreatedAt,
	}

	c.AgentSessionID = ""
	c.History = nil
	c.Name = name
	c.UpdatedAt = time.Now()
	return slotID
}

// SwitchSession swaps the current active session with a saved one.
// target can be a slot ID or a session name.
func (c *ConversationContext) SwitchSession(target string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var matchID string
	for id, slot := range c.SavedSessions {
		if id == target || slot.Name == target {
			matchID = id
			break
		}
	}
	if matchID == "" {
		return fmt.Errorf("session %q not found", target)
	}

	saved := c.SavedSessions[matchID]

	newSlotID := c.nextSlotID()
	c.SavedSessions[newSlotID] = &SessionSlot{
		ID:             newSlotID,
		Name:           c.Name,
		AgentSessionID: c.AgentSessionID,
		History:        append([]HistoryEntry(nil), c.History...),
		CreatedAt:      c.CreatedAt,
	}

	c.AgentSessionID = saved.AgentSessionID
	c.Name = saved.Name
	c.History = append([]HistoryEntry(nil), saved.History...)
	c.CreatedAt = saved.CreatedAt

	delete(c.SavedSessions, matchID)
	c.UpdatedAt = time.Now()
	return nil
}

// ListSessions returns all sessions (current + saved).
func (c *ConversationContext) ListSessions() []SessionSlot {
	c.mu.Lock()
	defer c.mu.Unlock()

	result := make([]SessionSlot, 0, 1+len(c.SavedSessions))
	result = append(result, SessionSlot{
		ID:             "current",
		Name:           c.Name,
		AgentSessionID: c.AgentSessionID,
		History:        c.History,
		CreatedAt:      c.CreatedAt,
	})
	for _, s := range c.SavedSessions {
		result = append(result, *s)
	}
	return result
}

// ConversationStore manages all ConversationContexts with persistence.
type ConversationStore struct {
	mu           sync.RWMutex
	contexts     map[string]*ConversationContext
	sessionNames map[string]string // agent session ID → custom display name
	storePath    string
}

func NewConversationStore(storePath string) *ConversationStore {
	cs := &ConversationStore{
		contexts:     make(map[string]*ConversationContext),
		sessionNames: make(map[string]string),
		storePath:    storePath,
	}
	if storePath != "" {
		cs.load()
	}
	return cs
}

// SetSessionName sets a custom display name for an agent session ID.
func (cs *ConversationStore) SetSessionName(agentSessionID, name string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if name == "" {
		delete(cs.sessionNames, agentSessionID)
	} else {
		cs.sessionNames[agentSessionID] = name
	}
}

// GetSessionName returns the custom name for an agent session, or "".
func (cs *ConversationStore) GetSessionName(agentSessionID string) string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.sessionNames[agentSessionID]
}

func (cs *ConversationStore) GetOrCreate(key string) *ConversationContext {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if c, ok := cs.contexts[key]; ok {
		return c
	}
	now := time.Now()
	c := &ConversationContext{
		Key:           key,
		ApprovalPhase: PhasePlanning,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	cs.contexts[key] = c
	return c
}

func (cs *ConversationStore) Get(key string) *ConversationContext {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.contexts[key]
}

func (cs *ConversationStore) List() []*ConversationContext {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	result := make([]*ConversationContext, 0, len(cs.contexts))
	for _, c := range cs.contexts {
		result = append(result, c)
	}
	return result
}

func (cs *ConversationStore) Delete(key string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	delete(cs.contexts, key)
}

// conversationSnapshot is the JSON-serializable state.
type conversationSnapshot struct {
	Contexts     []*ConversationContext `json:"contexts"`
	SessionNames map[string]string      `json:"session_names,omitempty"`
}

func (cs *ConversationStore) Save() {
	if cs.storePath == "" {
		return
	}
	cs.mu.RLock()
	snap := conversationSnapshot{
		Contexts:     make([]*ConversationContext, 0, len(cs.contexts)),
		SessionNames: cs.sessionNames,
	}
	for _, c := range cs.contexts {
		c.mu.Lock()
		cp := *c
		cp.mu = sync.Mutex{}
		c.mu.Unlock()
		snap.Contexts = append(snap.Contexts, &cp)
	}
	cs.mu.RUnlock()

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		slog.Error("conversation store: marshal failed", "error", err)
		return
	}
	if err := AtomicWriteFile(cs.storePath, data, 0644); err != nil {
		slog.Error("conversation store: write failed", "error", err, "path", cs.storePath)
	}
}

func (cs *ConversationStore) load() {
	data, err := os.ReadFile(cs.storePath)
	if err != nil {
		return
	}
	var snap conversationSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		slog.Warn("conversation store: corrupt file, starting fresh", "error", err, "path", cs.storePath)
		return
	}
	for _, c := range snap.Contexts {
		if c.Key != "" {
			cs.contexts[c.Key] = c
		}
	}
	if snap.SessionNames != nil {
		cs.sessionNames = snap.SessionNames
	}
}
