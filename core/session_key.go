package core

import "strings"

// SessionKey is a typed, structured representation of a session identifier.
// It replaces all hand-crafted fmt.Sprintf/strings.Split session key manipulation.
type SessionKey struct {
	Platform string
	ChatID   string
	UserID   string // empty for shared-channel sessions
}

func ParseSessionKey(raw string) SessionKey {
	parts := strings.SplitN(raw, ":", 3)
	switch len(parts) {
	case 3:
		return SessionKey{Platform: parts[0], ChatID: parts[1], UserID: parts[2]}
	case 2:
		return SessionKey{Platform: parts[0], ChatID: parts[1]}
	default:
		return SessionKey{Platform: raw}
	}
}

func (k SessionKey) String() string {
	if k.UserID == "" {
		return k.Platform + ":" + k.ChatID
	}
	return k.Platform + ":" + k.ChatID + ":" + k.UserID
}

// PrivateKey returns a session key for sending private (P2P) messages to the user.
// Uses UserID as both ChatID and UserID so the message goes to the user's private chat.
// Falls back to String() if UserID is empty.
func (k SessionKey) PrivateKey() string {
	if k.UserID == "" {
		return k.String()
	}
	return k.Platform + ":" + k.UserID + ":" + k.UserID
}

// WithUserID returns a copy with a different UserID, keeping the same Platform and ChatID.
func (k SessionKey) WithUserID(userID string) SessionKey {
	return SessionKey{Platform: k.Platform, ChatID: k.ChatID, UserID: userID}
}
