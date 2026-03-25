package core

import (
	"sort"
	"testing"
)

func TestAgentPool_RegisterAndList(t *testing.T) {
	pool := NewAgentPool()

	a1 := &stubAgent{}
	a2 := &stubAgent{}
	pool.Register("claude", a1)
	pool.Register("gemini", a2)

	names := pool.ListAgents()
	sort.Strings(names)
	if len(names) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(names))
	}
	if names[0] != "claude" || names[1] != "gemini" {
		t.Fatalf("unexpected agent names: %v", names)
	}

	// First registered agent becomes default
	if pool.ActiveName("any-session") != "claude" {
		t.Fatalf("expected default agent to be 'claude', got %q", pool.ActiveName("any-session"))
	}
}

func TestAgentPool_GetActiveAgent(t *testing.T) {
	pool := NewAgentPool()

	a1 := &stubAgent{}
	a2 := &stubAgent{}
	pool.Register("claude", a1)
	pool.Register("gemini", a2)

	// Default agent for any session
	got := pool.GetAgent("user-1")
	if got != a1 {
		t.Fatal("expected default agent (claude)")
	}

	// Change default
	if !pool.SetDefault("gemini") {
		t.Fatal("SetDefault should succeed for registered agent")
	}
	got = pool.GetAgent("user-1")
	if got != a2 {
		t.Fatal("expected new default agent (gemini)")
	}

	// SetDefault with unknown agent should fail
	if pool.SetDefault("unknown") {
		t.Fatal("SetDefault should fail for unknown agent")
	}
}

func TestAgentPool_SetPerUser(t *testing.T) {
	pool := NewAgentPool()

	a1 := &stubAgent{}
	a2 := &stubAgent{}
	pool.Register("claude", a1)
	pool.Register("gemini", a2)

	// Set per-user agent
	if !pool.SetUserAgent("user-1", "gemini") {
		t.Fatal("SetUserAgent should succeed for registered agent")
	}
	got := pool.GetAgent("user-1")
	if got != a2 {
		t.Fatal("expected per-user agent (gemini) for user-1")
	}

	// Other users still get default
	got = pool.GetAgent("user-2")
	if got != a1 {
		t.Fatal("expected default agent (claude) for user-2")
	}

	// ActiveName reflects per-user override
	if pool.ActiveName("user-1") != "gemini" {
		t.Fatalf("expected ActiveName='gemini' for user-1, got %q", pool.ActiveName("user-1"))
	}
	if pool.ActiveName("user-2") != "claude" {
		t.Fatalf("expected ActiveName='claude' for user-2, got %q", pool.ActiveName("user-2"))
	}

	// SetUserAgent with unknown agent should fail
	if pool.SetUserAgent("user-1", "unknown") {
		t.Fatal("SetUserAgent should fail for unknown agent")
	}
}

func TestAgentPool_SwitchAgent(t *testing.T) {
	pool := NewAgentPool()

	a1 := &stubAgent{}
	a2 := &stubAgent{}
	a3 := &stubAgent{}
	pool.Register("claude", a1)
	pool.Register("gemini", a2)
	pool.Register("gpt", a3)

	// Switch user-1 to gemini
	if !pool.SetUserAgent("user-1", "gemini") {
		t.Fatal("switch to gemini should succeed")
	}
	if pool.ActiveName("user-1") != "gemini" {
		t.Fatal("user-1 should be on gemini")
	}

	// Switch user-1 to gpt
	if !pool.SetUserAgent("user-1", "gpt") {
		t.Fatal("switch to gpt should succeed")
	}
	if pool.ActiveName("user-1") != "gpt" {
		t.Fatal("user-1 should be on gpt")
	}
	if pool.GetAgent("user-1") != a3 {
		t.Fatal("user-1 should get gpt agent")
	}

	// user-2 still on default (claude)
	if pool.GetAgent("user-2") != a1 {
		t.Fatal("user-2 should still get default (claude)")
	}
}
