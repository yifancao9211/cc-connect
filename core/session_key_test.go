package core

import "testing"

func TestParseSessionKey_ThreePart(t *testing.T) {
	sk := ParseSessionKey("feishu:oc_chat123:ou_user456")
	if sk.Platform != "feishu" {
		t.Errorf("Platform = %q, want feishu", sk.Platform)
	}
	if sk.ChatID != "oc_chat123" {
		t.Errorf("ChatID = %q, want oc_chat123", sk.ChatID)
	}
	if sk.UserID != "ou_user456" {
		t.Errorf("UserID = %q, want ou_user456", sk.UserID)
	}
}

func TestParseSessionKey_TwoPart(t *testing.T) {
	sk := ParseSessionKey("feishu:oc_chat123")
	if sk.Platform != "feishu" {
		t.Errorf("Platform = %q, want feishu", sk.Platform)
	}
	if sk.ChatID != "oc_chat123" {
		t.Errorf("ChatID = %q, want oc_chat123", sk.ChatID)
	}
	if sk.UserID != "" {
		t.Errorf("UserID = %q, want empty", sk.UserID)
	}
}

func TestParseSessionKey_Relay(t *testing.T) {
	sk := ParseSessionKey("relay:project1:chat1")
	if sk.Platform != "relay" || sk.ChatID != "project1" || sk.UserID != "chat1" {
		t.Errorf("got %+v", sk)
	}
}

func TestSessionKey_String(t *testing.T) {
	tests := []struct {
		name string
		key  SessionKey
		want string
	}{
		{"three-part", SessionKey{"feishu", "oc_chat", "ou_user"}, "feishu:oc_chat:ou_user"},
		{"two-part (shared channel)", SessionKey{"feishu", "oc_chat", ""}, "feishu:oc_chat"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.key.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionKey_RoundTrip(t *testing.T) {
	originals := []string{
		"feishu:oc_abc:ou_xyz",
		"feishu:oc_abc",
		"telegram:12345:67890",
		"relay:proj:chat",
	}
	for _, orig := range originals {
		sk := ParseSessionKey(orig)
		if got := sk.String(); got != orig {
			t.Errorf("RoundTrip(%q) = %q", orig, got)
		}
	}
}

func TestSessionKey_PrivateKey(t *testing.T) {
	sk := SessionKey{Platform: "feishu", ChatID: "oc_groupchat", UserID: "ou_admin123"}
	priv := sk.PrivateKey()
	want := "feishu:ou_admin123:ou_admin123"
	if priv != want {
		t.Errorf("PrivateKey() = %q, want %q", priv, want)
	}
}

func TestSessionKey_PrivateKey_NoUserID(t *testing.T) {
	sk := SessionKey{Platform: "feishu", ChatID: "oc_chat", UserID: ""}
	priv := sk.PrivateKey()
	if priv != "feishu:oc_chat" {
		t.Errorf("PrivateKey() with no UserID = %q, want fallback to String()", priv)
	}
}

func TestSessionKey_WithUserID(t *testing.T) {
	sk := SessionKey{Platform: "feishu", ChatID: "oc_group", UserID: "ou_user1"}
	newSK := sk.WithUserID("ou_admin")
	if newSK.String() != "feishu:oc_group:ou_admin" {
		t.Errorf("WithUserID = %q, want feishu:oc_group:ou_admin", newSK.String())
	}
	if sk.UserID != "ou_user1" {
		t.Error("original should not be modified")
	}
}
