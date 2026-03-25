package core

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// ApprovalPhase represents the current phase of the approval workflow.
type ApprovalPhase int

const (
	PhasePlanning ApprovalPhase = iota // Agent is in plan mode, analyzing
	PhasePending                       // Plan submitted, waiting for admin
	PhaseExecuting                     // Approved, Agent executing with approve-all
	PhaseCompleted                     // Execution done, about to reset
)

func (p ApprovalPhase) String() string {
	switch p {
	case PhasePlanning:
		return "planning"
	case PhasePending:
		return "pending"
	case PhaseExecuting:
		return "executing"
	case PhaseCompleted:
		return "completed"
	default:
		return "unknown"
	}
}

// ── Admin helpers ─────────────────────────────────────────────

// IsAdmin checks if userID is in the admin list (comma-separated, "*" = all).
func IsAdmin(adminFrom, userID string) bool {
	af := strings.TrimSpace(adminFrom)
	if af == "" {
		return false
	}
	if af == "*" {
		return true
	}
	for _, id := range strings.Split(af, ",") {
		if strings.EqualFold(strings.TrimSpace(id), userID) {
			return true
		}
	}
	return false
}

// NotifyUser sends a proactive message to a user by reconstructing their reply context.
func (e *Engine) NotifyUser(targetSessionKey, message string) error {
	sk := ParseSessionKey(targetSessionKey)
	if sk.ChatID == "" {
		return fmt.Errorf("invalid session key: %s", targetSessionKey)
	}
	platformName := sk.Platform

	var targetPlatform Platform
	for _, p := range e.platforms {
		if p.Name() == platformName {
			targetPlatform = p
			break
		}
	}
	if targetPlatform == nil {
		return fmt.Errorf("platform %q not found", platformName)
	}

	rc, ok := targetPlatform.(ReplyContextReconstructor)
	if !ok {
		return fmt.Errorf("platform %q does not support proactive messaging", platformName)
	}

	replyCtx, err := rc.ReconstructReplyCtx(targetSessionKey)
	if err != nil {
		return fmt.Errorf("reconstruct reply context: %w", err)
	}

	slog.Info("notify: sending to user", "target", targetSessionKey)
	return targetPlatform.Send(e.ctx, replyCtx, message)
}

// NotifySessionCompletion sends a completion notification to both owner and reviewer.
func (e *Engine) NotifySessionCompletion(sessionKey, summary string) {
	conv := e.conversations.Get(sessionKey)
	if conv == nil {
		return
	}

	now := time.Now().Format("15:04")

	conv.mu.Lock()
	owner := conv.OwnerID
	reviewerID := conv.ReviewerID
	conv.mu.Unlock()

	if owner != "" {
		ownerKey := buildPrivateSessionKey(sessionKey, owner)
		if ownerKey != sessionKey {
			msg := fmt.Sprintf("✅ [%s] 你的任务已完成:\n%s", now, truncateStr(summary, 500))
			if err := e.NotifyUser(ownerKey, msg); err != nil {
				slog.Warn("notify: failed to notify owner", "owner", owner, "error", err)
			}
		}
	}

	if reviewerID != "" {
		reviewerKey := buildPrivateSessionKey(sessionKey, reviewerID)
		if reviewerKey != sessionKey {
			msg := fmt.Sprintf("✅ [%s] 审批的任务已完成:\n%s", now, truncateStr(summary, 500))
			if err := e.NotifyUser(reviewerKey, msg); err != nil {
				slog.Warn("notify: failed to notify reviewer", "reviewer", reviewerID, "error", err)
			}
		}
	}
}

// findSessionKeyByUserID rebuilds a session key with a different userID.
// Keeps the original chatID, so messages go to the same chat (group).
func findSessionKeyByUserID(refSessionKey, targetUserID string) string {
	sk := ParseSessionKey(refSessionKey)
	if sk.UserID != "" {
		return sk.WithUserID(targetUserID).String()
	}
	return refSessionKey
}

// buildPrivateSessionKey builds a session key for private (P2P) messaging.
// Uses targetUserID as BOTH chatID and userID, so the message is sent to
// the user's private chat instead of a group chat.
func buildPrivateSessionKey(refSessionKey, targetUserID string) string {
	sk := ParseSessionKey(refSessionKey)
	if sk.Platform == "" {
		return refSessionKey
	}
	return SessionKey{Platform: sk.Platform, ChatID: targetUserID, UserID: targetUserID}.String()
}
