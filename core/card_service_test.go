package core

import "testing"

func TestCardService_RegisterAndHandleNav(t *testing.T) {
	cs := NewCardService()

	called := false
	cs.RegisterNav("/help", func(args, sessionKey string) *Card {
		called = true
		if args != "general" {
			t.Errorf("args = %q, want %q", args, "general")
		}
		if sessionKey != "feishu:chat1:user1" {
			t.Errorf("sessionKey = %q, want %q", sessionKey, "feishu:chat1:user1")
		}
		return &Card{Header: &CardHeader{Title: "Help"}}
	})

	card := cs.HandleNav("/help", "general", "feishu:chat1:user1")
	if !called {
		t.Fatal("handler was not called")
	}
	if card == nil {
		t.Fatal("expected non-nil card")
	}
	if card.Header.Title != "Help" {
		t.Errorf("card title = %q, want %q", card.Header.Title, "Help")
	}
}

func TestCardService_RegisterAndHandleAct(t *testing.T) {
	cs := NewCardService()

	cs.RegisterAct("/model", func(args, sessionKey string) *Card {
		return &Card{Header: &CardHeader{Title: "Model: " + args}}
	})

	card := cs.HandleAct("/model", "gpt-4", "slack:ch1:u1")
	if card == nil {
		t.Fatal("expected non-nil card")
	}
	if card.Header.Title != "Model: gpt-4" {
		t.Errorf("card title = %q, want %q", card.Header.Title, "Model: gpt-4")
	}

	// Nav handler should not find act registrations
	if c := cs.HandleNav("/model", "gpt-4", "slack:ch1:u1"); c != nil {
		t.Error("nav should not find act handler")
	}
}

func TestCardService_NotFound(t *testing.T) {
	cs := NewCardService()

	if c := cs.HandleNav("/missing", "", "k"); c != nil {
		t.Error("expected nil for unregistered nav path")
	}
	if c := cs.HandleAct("/missing", "", "k"); c != nil {
		t.Error("expected nil for unregistered act path")
	}
}
