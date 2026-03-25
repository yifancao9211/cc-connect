package core

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Bot-to-bot relay
// ──────────────────────────────────────────────────────────────

// HandleRelay processes a relay message synchronously: starts or resumes a
// dedicated relay session, sends the message to the agent, and blocks until
// the complete response is collected.
func (e *Engine) HandleRelay(ctx context.Context, fromProject, chatID, message string) (string, error) {
	relaySessionKey := "relay:" + fromProject + ":" + chatID
	conv := e.conversations.GetOrCreate(relaySessionKey)

	if inj, ok := e.agent.(SessionEnvInjector); ok {
		envVars := []string{
			"CC_PROJECT=" + e.name,
			"CC_SESSION_KEY=" + relaySessionKey,
		}
		if exePath, err := os.Executable(); err == nil {
			binDir := filepath.Dir(exePath)
			if curPath := os.Getenv("PATH"); curPath != "" {
				envVars = append(envVars, "PATH="+binDir+string(filepath.ListSeparator)+curPath)
			}
		}
		inj.SetSessionEnv(envVars)
	}

	agentSession, err := e.agent.StartSession(ctx, conv.AgentSessionID)
	if err != nil {
		return "", fmt.Errorf("start relay session: %w", err)
	}
	defer agentSession.Close()

	if conv.CompareAndSetAgentSessionID(agentSession.CurrentSessionID()) {
		e.conversations.Save()
	}

	if err := agentSession.Send(message, nil, nil); err != nil {
		return "", fmt.Errorf("send relay message: %w", err)
	}

	var textParts []string
	for event := range agentSession.Events() {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		switch event.Type {
		case EventText:
			if event.Content != "" {
				textParts = append(textParts, event.Content)
			}
			if event.SessionID != "" {
				if conv.CompareAndSetAgentSessionID(event.SessionID) {
					e.conversations.Save()
				}
			}
		case EventResult:
			if event.SessionID != "" {
				conv.SetAgentSessionID(event.SessionID)
				e.conversations.Save()
			}
			resp := event.Content
			if resp == "" && len(textParts) > 0 {
				resp = strings.Join(textParts, "")
			}
			if resp == "" {
				resp = "(empty response)"
			}
			slog.Info("relay: turn complete", "from", fromProject, "to", e.name, "response_len", len(resp))
			return resp, nil
		case EventError:
			if event.Error != nil {
				return "", event.Error
			}
			return "", fmt.Errorf("agent error (no details)")
		case EventPermissionRequest:
			_ = agentSession.RespondPermission(event.RequestID, PermissionResult{
				Behavior:     "allow",
				UpdatedInput: event.ToolInputRaw,
			})
		}
	}

	if len(textParts) > 0 {
		return strings.Join(textParts, ""), nil
	}
	return "", fmt.Errorf("relay: agent process exited without response")
}

// cmdBind handles /bind — establishes a relay binding between bots in a group chat.
//
// Usage:
//
//	/bind <project>           — bind current bot with another project in this group
//	/bind remove              — remove all bindings for this group
//	/bind -<project>          — remove specific project from binding
//	/bind                     — show current binding status
//
// The <project> argument is the project name from config.toml [[projects]].
// Multiple projects can be bound together for relay.
func (e *Engine) cmdBind(p Platform, msg *Message, args []string) {
	if e.relayManager == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRelayNotAvailable))
		return
	}

	_, chatID, err := parseSessionKeyParts(msg.SessionKey)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRelayNotAvailable))
		return
	}

	if len(args) == 0 {
		e.cmdBindStatus(p, msg.ReplyCtx, chatID)
		return
	}

	otherProject := args[0]

	// Handle removal commands
	if otherProject == "remove" || otherProject == "rm" || otherProject == "unbind" || otherProject == "del" || otherProject == "clear" {
		e.relayManager.Unbind(chatID)
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRelayUnbound))
		return
	}

	if otherProject == "setup" {
		e.cmdBindSetup(p, msg)
		return
	}

	if otherProject == "help" || otherProject == "-h" || otherProject == "--help" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRelayUsage))
		return
	}

	// Handle removal with - prefix: /bind -project
	if strings.HasPrefix(otherProject, "-") {
		projectToRemove := strings.TrimPrefix(otherProject, "-")
		if e.relayManager.RemoveFromBind(chatID, projectToRemove) {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgRelayBindRemoved), projectToRemove))
		} else {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgRelayBindNotFound), projectToRemove))
		}
		return
	}

	if otherProject == e.name {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRelayBindSelf))
		return
	}

	// Validate the target project exists
	if !e.relayManager.HasEngine(otherProject) {
		available := e.relayManager.ListEngineNames()
		var others []string
		for _, n := range available {
			if n != e.name {
				others = append(others, n)
			}
		}
		if len(others) == 0 {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgRelayNoTarget), otherProject))
		} else {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgRelayNotFound), otherProject, strings.Join(others, ", ")))
		}
		return
	}

	// Add current project and target project to binding
	e.relayManager.AddToBind(p.Name(), chatID, e.name)
	e.relayManager.AddToBind(p.Name(), chatID, otherProject)

	// Get all bound projects for status message
	binding := e.relayManager.GetBinding(chatID)
	var boundProjects []string
	for proj := range binding.Bots {
		boundProjects = append(boundProjects, proj)
	}

	reply := fmt.Sprintf(e.i18n.T(MsgRelayBindSuccess), strings.Join(boundProjects, " ↔ "), otherProject, otherProject)

	if _, ok := e.agent.(SystemPromptSupporter); !ok {
		if mp, ok := e.agent.(MemoryFileProvider); ok {
			reply += fmt.Sprintf(e.i18n.T(MsgRelaySetupHint), filepath.Base(mp.ProjectMemoryFile()))
		}
	}

	e.reply(p, msg.ReplyCtx, reply)
}

func (e *Engine) cmdBindStatus(p Platform, replyCtx any, chatID string) {
	binding := e.relayManager.GetBinding(chatID)
	if binding == nil {
		e.reply(p, replyCtx, e.i18n.T(MsgRelayNoBinding))
		return
	}
	var parts []string
	for proj := range binding.Bots {
		parts = append(parts, proj)
	}
	e.reply(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgRelayBound), strings.Join(parts, " ↔ ")))
}

const ccConnectInstructionMarker = "<!-- cc-connect-instructions -->"

func (e *Engine) cmdBindSetup(p Platform, msg *Message) {
	mp, ok := e.agent.(MemoryFileProvider)
	if !ok {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRelaySetupNoMemory))
		return
	}

	filePath := mp.ProjectMemoryFile()
	if filePath == "" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRelaySetupNoMemory))
		return
	}

	existing, _ := os.ReadFile(filePath)
	if strings.Contains(string(existing), ccConnectInstructionMarker) {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgRelaySetupExists), filepath.Base(filePath)))
		return
	}

	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v", err))
		return
	}

	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v", err))
		return
	}
	defer f.Close()

	block := "\n" + ccConnectInstructionMarker + "\n" + AgentSystemPrompt() + "\n"
	if _, err := f.WriteString(block); err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v", err))
		return
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgRelaySetupOK), filepath.Base(filePath)))
}

func extractChannelID(sessionKey string) string {
	return ParseSessionKey(sessionKey).ChatID
}

// commandContext resolves the appropriate agent and interactive key
// for a command. In multi-workspace mode, it routes to the bound workspace if present.
func (e *Engine) commandContext(p Platform, msg *Message) (Agent, string, error) {
	if !e.multiWorkspace {
		return e.agent, msg.SessionKey, nil
	}
	channelID := extractChannelID(msg.SessionKey)
	if channelID == "" {
		return e.agent, msg.SessionKey, nil
	}
	workspace, _, err := e.resolveWorkspace(p, channelID)
	if err != nil {
		return nil, "", err
	}
	if workspace == "" {
		return e.agent, msg.SessionKey, nil
	}
	wsAgent, err := e.getOrCreateWorkspaceAgent(workspace)
	if err != nil {
		return nil, "", err
	}
	return wsAgent, workspace + ":" + msg.SessionKey, nil
}

// sessionContextForKey resolves the agent for a sessionKey.
// It uses existing workspace bindings and falls back to global context if unresolved.
func (e *Engine) sessionContextForKey(sessionKey string) Agent {
	if !e.multiWorkspace || e.workspaceBindings == nil {
		return e.agent
	}
	channelID := extractChannelID(sessionKey)
	if channelID == "" {
		return e.agent
	}
	projectKey := "project:" + e.name
	if b := e.workspaceBindings.Lookup(projectKey, channelID); b != nil {
		if wsAgent, err := e.getOrCreateWorkspaceAgent(b.Workspace); err == nil {
			return wsAgent
		}
	}
	return e.agent
}

// interactiveKeyForSessionKey returns the interactive state key for a sessionKey.
// In multi-workspace mode, it prefixes with the bound workspace path when available.
func (e *Engine) interactiveKeyForSessionKey(sessionKey string) string {
	if !e.multiWorkspace || e.workspaceBindings == nil {
		return sessionKey
	}
	channelID := extractChannelID(sessionKey)
	if channelID == "" {
		return sessionKey
	}
	projectKey := "project:" + e.name
	if b := e.workspaceBindings.Lookup(projectKey, channelID); b != nil {
		return b.Workspace + ":" + sessionKey
	}
	return sessionKey
}

// resolveWorkspace resolves a channel to a workspace directory.
// Returns (workspacePath, channelName, error).
// If workspacePath is empty, the init flow should be triggered.
func (e *Engine) resolveWorkspace(p Platform, channelID string) (string, string, error) {
	projectKey := "project:" + e.name

	// Step 1: Check existing binding
	if b := e.workspaceBindings.Lookup(projectKey, channelID); b != nil {
		// Verify workspace directory still exists
		if _, err := os.Stat(b.Workspace); err != nil {
			slog.Warn("bound workspace directory missing, removing binding",
				"workspace", b.Workspace, "channel", channelID)
			e.workspaceBindings.Unbind(projectKey, channelID)
			return "", b.ChannelName, nil
		}
		return b.Workspace, b.ChannelName, nil
	}

	// Step 2: Resolve channel name for convention match
	channelName := ""
	if resolver, ok := p.(ChannelNameResolver); ok {
		name, err := resolver.ResolveChannelName(channelID)
		if err != nil {
			slog.Warn("failed to resolve channel name", "channel", channelID, "err", err)
		} else {
			channelName = name
		}
	}

	if channelName == "" {
		return "", "", nil
	}

	// Step 3: Convention match — check if base_dir/<channel-name> exists
	candidate := filepath.Join(e.baseDir, channelName)
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		// Auto-bind
		e.workspaceBindings.Bind(projectKey, channelID, channelName, candidate)
		slog.Info("workspace auto-bound by convention",
			"channel", channelName, "workspace", candidate)
		return candidate, channelName, nil
	}

	return "", channelName, nil
}

// handleWorkspaceInitFlow manages the conversational workspace setup.
// Returns true if the message was consumed by the init flow.
func (e *Engine) handleWorkspaceInitFlow(p Platform, msg *Message, channelID, channelName string) bool {
	e.initFlowsMu.Lock()
	flow, exists := e.initFlows[channelID]
	e.initFlowsMu.Unlock()

	content := strings.TrimSpace(msg.Content)

	if !exists {
		if strings.HasPrefix(content, "/") {
			return false
		}
		e.initFlowsMu.Lock()
		e.initFlows[channelID] = &workspaceInitFlow{
			state:       "awaiting_url",
			channelName: channelName,
		}
		e.initFlowsMu.Unlock()
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgWsNotFoundHint))
		return true
	}

	switch flow.state {
	case "awaiting_url":
		if !looksLikeGitURL(content) {
			e.reply(p, msg.ReplyCtx, "That doesn't look like a git URL. Please provide a URL like `https://github.com/org/repo` or `git@github.com:org/repo.git`.")
			return true
		}
		repoName := extractRepoName(content)
		cloneTo := filepath.Join(e.baseDir, repoName)

		e.initFlowsMu.Lock()
		flow.repoURL = content
		flow.cloneTo = cloneTo
		flow.state = "awaiting_confirm"
		e.initFlowsMu.Unlock()

		e.reply(p, msg.ReplyCtx, fmt.Sprintf(
			"I'll clone `%s` to `%s` and bind it to this channel. OK? (yes/no)", content, cloneTo))
		return true

	case "awaiting_confirm":
		lower := strings.ToLower(content)
		if lower != "yes" && lower != "y" {
			e.initFlowsMu.Lock()
			delete(e.initFlows, channelID)
			e.initFlowsMu.Unlock()
			e.reply(p, msg.ReplyCtx, "Cancelled. Send a repo URL anytime to try again.")
			return true
		}

		e.reply(p, msg.ReplyCtx, fmt.Sprintf("Cloning `%s` to `%s`...", flow.repoURL, flow.cloneTo))

		if err := gitClone(flow.repoURL, flow.cloneTo); err != nil {
			e.initFlowsMu.Lock()
			delete(e.initFlows, channelID)
			e.initFlowsMu.Unlock()
			e.reply(p, msg.ReplyCtx, fmt.Sprintf("Clone failed: %v\nSend a repo URL to try again.", err))
			return true
		}

		projectKey := "project:" + e.name
		e.workspaceBindings.Bind(projectKey, channelID, flow.channelName, flow.cloneTo)

		e.initFlowsMu.Lock()
		delete(e.initFlows, channelID)
		e.initFlowsMu.Unlock()

		e.reply(p, msg.ReplyCtx, fmt.Sprintf(
			"Clone complete. Bound workspace `%s` to this channel. Ready.", flow.cloneTo))
		return true
	}

	return false
}

func looksLikeGitURL(s string) bool {
	return strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "http://") ||
		strings.HasPrefix(s, "git@") ||
		strings.HasPrefix(s, "ssh://")
}

func extractRepoName(url string) string {
	url = strings.TrimSuffix(url, ".git")
	// Handle git@host:org/repo format
	if idx := strings.LastIndex(url, ":"); idx != -1 && strings.HasPrefix(url, "git@") {
		remainder := url[idx+1:]
		parts := strings.Split(remainder, "/")
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}
	// Handle https://host/org/repo format
	parts := strings.Split(url, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return "workspace"
}

// ── Admin Session Routing ───────────────────────────────────────
