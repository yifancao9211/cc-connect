package core

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)


// SetAdminOverride makes admin's messages route to targetSessionKey.
// When admin sends messages, they are processed in the target session's context.
func (e *Engine) SetAdminOverride(adminSessionKey, targetSessionKey string) {
	e.adminOverridesMu.Lock()
	defer e.adminOverridesMu.Unlock()
	if e.adminOverrides == nil {
		e.adminOverrides = make(map[string]string)
	}
	e.adminOverrides[adminSessionKey] = targetSessionKey
	slog.Info("admin: entered session", "admin", adminSessionKey, "target", targetSessionKey)
}

// ClearAdminOverride removes admin's session routing override.
func (e *Engine) ClearAdminOverride(adminSessionKey string) {
	e.adminOverridesMu.Lock()
	defer e.adminOverridesMu.Unlock()
	delete(e.adminOverrides, adminSessionKey)
	slog.Info("admin: left session", "admin", adminSessionKey)
}

// GetAdminOverride returns the target session key if admin has an active override.
func (e *Engine) GetAdminOverride(adminSessionKey string) (string, bool) {
	e.adminOverridesMu.RLock()
	defer e.adminOverridesMu.RUnlock()
	target, ok := e.adminOverrides[adminSessionKey]
	return target, ok
}

// sendApprovalCardToAdmin sends an interactive approval card to the admin.
// Called when a write operation is blocked due to missing approval.
func (e *Engine) sendApprovalCardToAdmin(p Platform, sessionKey, userMessage, agentPlan string) {
	// Parse admin IDs from config
	adminIDs := strings.Split(e.adminFrom, ",")
	if len(adminIDs) == 0 || e.adminFrom == "" {
		return
	}

	owner := extractUserID(sessionKey)
	if owner == "" {
		owner = sessionKey
	}
	// Try to resolve display name from platform
	if resolver, ok := p.(UserNameResolver); ok && owner != "" && owner != sessionKey {
		if name := resolver.ResolveUserName(owner); name != "" && name != owner {
			owner = name + " (" + extractUserID(sessionKey) + ")"
		}
	}

	// Truncate for display
	if len([]rune(userMessage)) > 200 {
		userMessage = string([]rune(userMessage)[:200]) + "…"
	}
	if len([]rune(agentPlan)) > 800 {
		agentPlan = string([]rune(agentPlan)[:800]) + "\n…(截断)"
	}

	cb := NewCard().
		Title(e.i18n.T(MsgApprovalTitle), "orange").
		Markdownf(e.i18n.T(MsgApprovalUser), owner)
	if userMessage != "" {
		cb.Markdownf(e.i18n.T(MsgApprovalRequest), userMessage)
	}
	if agentPlan != "" {
		cb.Divider()
		cb.Markdownf(e.i18n.T(MsgApprovalPlan), agentPlan)
	}
	card := cb.Divider().
		ButtonsEqual(
			PrimaryBtn(e.i18n.T(MsgApprovalBtnApprove), "act:approval/approve "+sessionKey),
			DefaultBtn(e.i18n.T(MsgApprovalBtnTrust), "act:approval/trust "+sessionKey),
			DangerBtn(e.i18n.T(MsgApprovalBtnReject), "act:approval/reject "+sessionKey),
		).
		Note(e.i18n.T(MsgApprovalNote)).
		Build()

	for _, adminID := range adminIDs {
		adminID = strings.TrimSpace(adminID)
		if adminID == "" || adminID == "*" {
			continue
		}

		// Build admin's private session key: use adminID as chatID for P2P messaging.
		// findSessionKeyByUserID only replaces userID but keeps the original chatID
		// (often a group chat), which would send approval to the group instead of
		// the admin's private chat.
		adminSessionKey := buildPrivateSessionKey(sessionKey, adminID)

		rc, ok := p.(ReplyContextReconstructor)
		if !ok {
			slog.Warn("approval: platform does not support proactive messaging", "platform", p.Name())
			continue
		}

		replyCtx, err := rc.ReconstructReplyCtx(adminSessionKey)
		if err != nil {
			slog.Warn("approval: failed to reconstruct admin reply context",
				"admin", adminID, "error", err)
			continue
		}

		if cs, ok := p.(CardSender); ok {
			if err := cs.SendCard(e.ctx, replyCtx, card); err != nil {
				slog.Error("approval: failed to send card to admin", "admin", adminID, "error", err)
			} else {
				slog.Info("approval: card sent to admin", "admin", adminID, "session", sessionKey)
			}
		} else {
			// Fallback: send as text
			_ = p.Send(e.ctx, replyCtx, card.RenderText())
		}
	}
}

// HandleSubmitPlan processes a submit-plan request from the Agent CLI.
// It transitions the session to PENDING and pushes an approval card to admins.
func (e *Engine) HandleSubmitPlan(sessionKey, planText string) error {
	conv := e.conversations.GetOrCreate(sessionKey)
	userMsg := conv.LastUserMessage()

	if !conv.SubmitPlan(userMsg, planText) {
		return fmt.Errorf("cannot submit plan: session is not in planning phase")
	}

	// Find the platform for this session to send the card
	platformName := ParseSessionKey(sessionKey).Platform
	var platform Platform
	for _, p := range e.platforms {
		if p.Name() == platformName {
			platform = p
			break
		}
	}

	if platform != nil {
		// Notify user
		if rc, ok := platform.(ReplyContextReconstructor); ok {
			if rctx, err := rc.ReconstructReplyCtx(sessionKey); err == nil {
				_ = platform.Send(e.ctx, rctx, e.i18n.T(MsgApprovalSubmitted))
			}
		}
		// Send card to admin
		go e.sendApprovalCardToAdmin(platform, sessionKey, userMsg, planText)
	}

	return nil
}

// handleApprovalCardAction processes approve/reject button clicks from the admin card.
// body format: "approve <sessionKey>" or "reject <sessionKey>"
// Returns a *Card to replace the original approval card in-place.
func (e *Engine) handleApprovalCardAction(body, callerSessionKey string) *Card {
	parts := strings.SplitN(body, " ", 2)
	if len(parts) < 2 {
		slog.Warn("approval: invalid card action", "body", body)
		return NewCard().Title("⚠️", "red").Markdown(e.i18n.T(MsgApprovalInvalidAction)).Build()
	}
	action, targetSessionKey := parts[0], parts[1]

	// Verify caller is admin
	callerUserID := extractUserID(callerSessionKey)
	if !e.isAdmin(callerUserID) {
		slog.Info("approval: non-admin clicked button", "user", callerUserID)
		return NewCard().Title("⚠️", "red").Markdown(e.i18n.T(MsgApprovalNoPermission)).Build()
	}

	conv := e.conversations.GetOrCreate(targetSessionKey)

	switch action {
	case "approve":
		if !conv.Approve(callerUserID, "") {
			slog.Warn("approval: approve failed (wrong phase)", "target", targetSessionKey)
			return NewCard().Title("⚠️", "red").Markdown(e.i18n.T(MsgApprovalWrongPhase)).Build()
		}
		slog.Info("approval: session approved via card", "target", targetSessionKey, "admin", callerUserID)

		go e.sendApprovedMessageToAgent(targetSessionKey)

		if conv.OwnerID != "" {
			ownerKey := buildPrivateSessionKey(targetSessionKey, conv.OwnerID)
			_ = e.NotifyUser(ownerKey, e.i18n.T(MsgApprovalNotifyOwner))
		}

		return NewCard().Title(e.i18n.T(MsgApprovalApproved), "green").Markdown(e.i18n.T(MsgApprovalApprovedBody)).Build()

	case "trust":
		if !conv.Approve(callerUserID, "") {
			slog.Warn("approval: trust failed (wrong phase)", "target", targetSessionKey)
			return NewCard().Title("⚠️", "red").Markdown(e.i18n.T(MsgApprovalWrongPhase)).Build()
		}
		conv.mu.Lock()
		conv.ApproveAll = true
		conv.mu.Unlock()
		slog.Info("approval: session trusted via card", "target", targetSessionKey, "admin", callerUserID)

		go e.sendApprovedMessageToAgent(targetSessionKey)

		if conv.OwnerID != "" {
			ownerKey := buildPrivateSessionKey(targetSessionKey, conv.OwnerID)
			_ = e.NotifyUser(ownerKey, e.i18n.T(MsgApprovalNotifyOwner))
		}

		return NewCard().Title(e.i18n.T(MsgApprovalTrusted), "turquoise").Markdown(e.i18n.T(MsgApprovalTrustedBody)).Build()

	case "reject":
		if !conv.Reject(callerUserID, "") {
			return NewCard().Title("⚠️", "red").Markdown(e.i18n.T(MsgApprovalWrongPhase)).Build()
		}
		slog.Info("approval: session rejected via card", "target", targetSessionKey, "admin", callerUserID)

		if conv.OwnerID != "" {
			ownerKey := buildPrivateSessionKey(targetSessionKey, conv.OwnerID)
			_ = e.NotifyUser(ownerKey, e.i18n.T(MsgApprovalRejectedBody))
		}

		return NewCard().Title(e.i18n.T(MsgApprovalRejected), "red").Markdown(e.i18n.T(MsgApprovalRejectedBody)).Build()
	}

	return nil
}


// extractUserID extracts the user ID from a session key.
// Session key format: "platform:chatID:userID" or "platform:chatID"
func extractUserID(sessionKey string) string {
	return ParseSessionKey(sessionKey).UserID
}


// cmdApprove marks the current reviewed session as approved.
// Admin-only command. Must be used while in another user's session (/switch).
func (e *Engine) cmdApprove(p Platform, msg *Message, args []string) {
	target, ok := e.GetAdminOverride(msg.SessionKey)
	if !ok {
		e.reply(p, msg.ReplyCtx, "❌ 你当前不在其他用户的会话中。请先用 /switch 进入要审批的会话。")
		return
	}

	conv := e.conversations.GetOrCreate(target)

	if !conv.Approve(msg.UserID, "") {
		e.reply(p, msg.ReplyCtx, "❌ 该会话当前不在待审批状态。")
		return
	}

	go e.sendApprovedMessageToAgent(target)

	if conv.OwnerID != "" {
		ownerKey := buildPrivateSessionKey(target, conv.OwnerID)
		if err := e.NotifyUser(ownerKey, e.i18n.T(MsgApprovalNotifyOwner)); err != nil {
			slog.Warn("approve: failed to notify owner", "owner", conv.OwnerID, "error", err)
		}
	}

	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgApprovalApprovedBody))
	slog.Info("approve: session approved", "target", target, "reviewer", msg.UserID)
}

// cmdReject marks the current reviewed session as rejected.
// Admin-only command.
func (e *Engine) cmdReject(p Platform, msg *Message, args []string) {
	target, ok := e.GetAdminOverride(msg.SessionKey)
	if !ok {
		e.reply(p, msg.ReplyCtx, "❌ 你当前不在其他用户的会话中。请先用 /switch 进入要审批的会话。")
		return
	}

	reason := "管理员拒绝"
	if len(args) > 0 {
		reason = strings.Join(args, " ")
	}

	conv := e.conversations.GetOrCreate(target)

	if !conv.Reject(msg.UserID, "") {
		e.reply(p, msg.ReplyCtx, "❌ 该会话当前不在待审批状态。")
		return
	}

	if conv.OwnerID != "" {
		ownerKey := buildPrivateSessionKey(target, conv.OwnerID)
		if err := e.NotifyUser(ownerKey, e.i18n.T(MsgApprovalRejectedBody)); err != nil {
			slog.Warn("reject: failed to notify owner", "owner", conv.OwnerID, "error", err)
		}
	}

	e.ClearAdminOverride(msg.SessionKey)
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgApprovalRejectedBody))
	slog.Info("reject: session rejected", "target", target, "reviewer", msg.UserID, "reason", reason)
}

// sendApprovedMessageToAgent sends an "approved, please execute" message
// to the Agent via the session, triggering the execution phase.
func (e *Engine) sendApprovedMessageToAgent(sessionKey string) {
	conv := e.conversations.Get(sessionKey)
	if conv == nil {
		slog.Warn("approval: no conversation found, cannot send approved message", "session", sessionKey)
		return
	}

	conv.mu.Lock()
	agentSession := conv.AgentSession
	conv.mu.Unlock()

	if agentSession == nil || !agentSession.Alive() {
		slog.Warn("approval: agent session is nil or closed, resetting to planning", "session", sessionKey)
		conv.CompleteExecution()
		return
	}

	msg := e.i18n.T(MsgApprovalExecuteMsg)
	if err := agentSession.Send(msg, nil, nil); err != nil {
		slog.Error("approval: failed to send approved message to agent", "session", sessionKey, "error", err)
		conv.CompleteExecution()
	} else {
		slog.Info("approval: sent execute message to agent", "session", sessionKey)
	}
}

// cmdLeave exits the admin from another user's session back to their own.
func (e *Engine) cmdLeave(p Platform, msg *Message) {
	target, ok := e.GetAdminOverride(msg.SessionKey)
	if !ok {
		e.reply(p, msg.ReplyCtx, "你当前不在其他用户的会话中。")
		return
	}

	e.ClearAdminOverride(msg.SessionKey)
	e.reply(p, msg.ReplyCtx, fmt.Sprintf("👋 已退出会话 [%s]，回到你自己的会话。", target))
}

func gitClone(repoURL, dest string) error {
	cmd := exec.Command("git", "clone", repoURL, dest)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}
