package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const maxPlatformMessageLen = 4000

const (
	defaultThinkingMaxLen = 300
	defaultToolMaxLen     = 500
)

// Slow-operation thresholds. Operations exceeding these durations produce a
// slog.Warn so operators can quickly pinpoint bottlenecks.
const (
	slowPlatformSend    = 2 * time.Second  // platform Reply / Send
	slowAgentStart      = 5 * time.Second  // agent.StartSession
	slowAgentClose      = 3 * time.Second  // agentSession.Close
	slowAgentSend       = 2 * time.Second  // agentSession.Send
	slowAgentFirstEvent = 15 * time.Second // time from send to first agent event
)

// VersionInfo is set by main at startup so that /version works.
var VersionInfo string

// CurrentVersion is the semver tag (e.g. "v1.2.0-beta.1"), set by main.
var CurrentVersion string

// RestartRequest carries info needed to send a post-restart notification.
type RestartRequest struct {
	SessionKey string `json:"session_key"`
	Platform   string `json:"platform"`
}

// SaveRestartNotify persists restart info so the new process can send
// a "restart successful" message after startup.
func SaveRestartNotify(dataDir string, req RestartRequest) error {
	dir := filepath.Join(dataDir, "run")
	os.MkdirAll(dir, 0o755)
	data, _ := json.Marshal(req)
	return os.WriteFile(filepath.Join(dir, "restart_notify"), data, 0o644)
}

// ConsumeRestartNotify reads and deletes the restart notification file.
// Returns nil if no notification is pending.
func ConsumeRestartNotify(dataDir string) *RestartRequest {
	p := filepath.Join(dataDir, "run", "restart_notify")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	os.Remove(p)
	var req RestartRequest
	if json.Unmarshal(data, &req) != nil {
		return nil
	}
	return &req
}

// SendRestartNotification sends a "restart successful" message to the
// platform/session that initiated the restart.
func (e *Engine) SendRestartNotification(platformName, sessionKey string) {
	for _, p := range e.platforms {
		if p.Name() != platformName {
			continue
		}
		rc, ok := p.(ReplyContextReconstructor)
		if !ok {
			slog.Debug("restart notify: platform does not support ReconstructReplyCtx", "platform", platformName)
			return
		}
		rctx, err := rc.ReconstructReplyCtx(sessionKey)
		if err != nil {
			slog.Debug("restart notify: reconstruct failed", "error", err)
			return
		}
		text := e.i18n.T(MsgRestartSuccess)
		if CurrentVersion != "" {
			text += fmt.Sprintf(" (%s)", CurrentVersion)
		}
		if err := p.Send(e.ctx, rctx, text); err != nil {
			slog.Debug("restart notify: send failed", "error", err)
		}
		return
	}
}

// RestartCh is signaled when /restart is invoked. main listens on it
// to perform a graceful shutdown followed by syscall.Exec.
var RestartCh = make(chan RestartRequest, 1)

// DisplayCfg controls truncation of intermediate messages.
// A value of -1 means "use default", 0 means "no truncation".
type DisplayCfg struct {
	ThinkingMaxLen int // max runes for thinking preview; 0 = no truncation
	ToolMaxLen     int // max runes for tool use preview; 0 = no truncation
}

// RateLimitCfg controls per-session message rate limiting.
type RateLimitCfg struct {
	MaxMessages int           // max messages per window; 0 = disabled
	Window      time.Duration // sliding window size
}

// Engine routes messages between platforms and the agent for a single project.
type Engine struct {
	name         string
	agent        Agent
	platforms    []Platform
	ctx          context.Context
	cancel       context.CancelFunc
	i18n         *I18n
	speech       SpeechCfg
	tts          *TTSCfg
	display      DisplayCfg
	defaultQuiet bool
	injectSender bool
	startedAt    time.Time

	providerSaveFunc       func(providerName string) error
	providerAddSaveFunc    func(p ProviderConfig) error
	providerRemoveSaveFunc func(name string) error

	ttsSaveFunc func(mode string) error

	commandSaveAddFunc func(name, description, prompt, exec, workDir string) error
	commandSaveDelFunc func(name string) error

	displaySaveFunc  func(thinkingMaxLen, toolMaxLen *int) error
	configReloadFunc func() (*ConfigReloadResult, error)

	cronScheduler *CronScheduler

	commands *CommandRegistry
	skills   *SkillRegistry
	aliases  map[string]string // trigger → command (e.g. "帮助" → "/help")
	aliasMu  sync.RWMutex

	aliasSaveAddFunc func(name, command string) error
	aliasSaveDelFunc func(name string) error

	bannedWords []string
	bannedMu    sync.RWMutex

	disabledCmds map[string]bool
	adminFrom    string // comma-separated user IDs for privileged commands; "*" = all allowed users; "" = deny

	rateLimiter      *RateLimiter
	streamPreview    StreamPreviewCfg
	relayManager     *RelayManager
	eventIdleTimeout time.Duration

	// Unified conversation state (replaces sessions + interactiveStates + approvalTracker)
	conversations *ConversationStore

	// Admin session routing: admin can "enter" another user's session
	adminOverrides   map[string]string    // adminSessionKey → targetSessionKey
	adminOverridesMu sync.RWMutex
	// Agent pool for multi-agent switching (optional, nil = single-agent mode)
	agentPool *AgentPool

	cardService *CardService

	// Multi-workspace mode
	multiWorkspace    bool
	baseDir           string
	workspaceBindings *WorkspaceBindingManager
	workspacePool     *workspacePool
	initFlows         map[string]*workspaceInitFlow // channelID → init state
	initFlowsMu       sync.Mutex

	quietMu sync.RWMutex
	quiet   bool // when true, suppress thinking and tool progress messages globally
}

// App is the new name for Engine. Use App in new code.
type App = Engine

// NewApp is the preferred constructor for new code. NewEngine is kept for backward compatibility.
var NewApp = NewEngine

// workspaceInitFlow tracks a channel that is being onboarded to a workspace.
type workspaceInitFlow struct {
	state       string // "awaiting_url", "awaiting_confirm"
	repoURL     string
	cloneTo     string
	channelName string
}

type deleteModeState struct {
	page        int
	selectedIDs map[string]struct{}
	phase       string
	hint        string
	result      string
}

// pendingPermission represents a permission request waiting for user response.
type pendingPermission struct {
	RequestID       string
	ToolName        string
	ToolInput       map[string]any
	InputPreview    string
	Questions       []UserQuestion // non-nil for AskUserQuestion
	Answers         map[int]string // collected answers keyed by question index
	CurrentQuestion int            // index of the question currently being asked
	Resolved        chan struct{}  // closed when user responds
	resolveOnce     sync.Once
}

// resolve safely closes the Resolved channel exactly once.
func (pp *pendingPermission) resolve() {
	pp.resolveOnce.Do(func() { close(pp.Resolved) })
}

func NewEngine(name string, ag Agent, platforms []Platform, sessionStorePath string, lang Language) *Engine {
	ctx, cancel := context.WithCancel(context.Background())

	convStorePath := ""
	if sessionStorePath != "" {
		convStorePath = strings.TrimSuffix(sessionStorePath, ".json") + "_conversations.json"
	}

	e := &Engine{
		name:              name,
		agent:             ag,
		platforms:         platforms,
		conversations:     NewConversationStore(convStorePath),
		ctx:               ctx,
		cancel:            cancel,
		i18n:              NewI18n(lang),
		display:           DisplayCfg{ThinkingMaxLen: defaultThinkingMaxLen, ToolMaxLen: defaultToolMaxLen},
		commands:          NewCommandRegistry(),
		skills:            NewSkillRegistry(),
		aliases:           make(map[string]string),
		startedAt:        time.Now(),
		streamPreview:    DefaultStreamPreviewCfg(),
		eventIdleTimeout: defaultEventIdleTimeout,
		adminOverrides:   make(map[string]string),
		cardService:      NewCardService(),
	}

	if cp, ok := ag.(CommandProvider); ok {
		e.commands.SetAgentDirs(cp.CommandDirs())
	}
	if sp, ok := ag.(SkillProvider); ok {
		e.skills.SetDirs(sp.SkillDirs())
	}

	e.registerBuiltinAliases()
	e.registerCardHandlers()

	return e
}

// registerBuiltinAliases adds hardcoded Chinese command aliases.
// These are always available regardless of config and use the same
// alias mechanism as user-defined aliases (trigger → /command).
func (e *Engine) registerBuiltinAliases() {
	builtinAliases := map[string]string{
		"帮助": "/help",
		"新建": "/new",
		"列表": "/list",
		"切换": "/switch",
		"模型": "/model",
		"引擎": "/agent",
		"停止": "/stop",
		"批准": "/approve",
		"拒绝": "/reject",
		"状态": "/status",
		"压缩": "/compress",
		"安静": "/quiet",
		"当前": "/current",
		"历史": "/history",
		"记忆": "/memory",
	}
	e.aliasMu.Lock()
	defer e.aliasMu.Unlock()
	for trigger, cmd := range builtinAliases {
		if _, exists := e.aliases[trigger]; !exists {
			e.aliases[trigger] = cmd
		}
	}
}

// SetMultiWorkspace enables multi-workspace mode for the engine.
func (e *Engine) SetMultiWorkspace(baseDir, bindingStorePath string) {
	e.multiWorkspace = true
	e.baseDir = baseDir
	e.workspaceBindings = NewWorkspaceBindingManager(bindingStorePath)
	e.workspacePool = newWorkspacePool(15 * time.Minute)
	e.initFlows = make(map[string]*workspaceInitFlow)
	go e.runIdleReaper()
}

func (e *Engine) runIdleReaper() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			if e.workspacePool == nil {
				continue
			}
			reaped := e.workspacePool.ReapIdle()
			for _, ws := range reaped {
				for _, conv := range e.conversations.List() {
					if conv.WorkspaceDir == ws {
						conv.mu.Lock()
						as := conv.AgentSession
						conv.mu.Unlock()
						if as != nil {
							as.Close()
						}
						conv.ClearRuntime()
					}
				}
				slog.Info("workspace idle-reaped", "workspace", ws)
			}
		}
	}
}

// SetSpeechConfig configures the speech-to-text subsystem.
func (e *Engine) SetSpeechConfig(cfg SpeechCfg) {
	e.speech = cfg
}

// SetTTSConfig configures the text-to-speech subsystem.
func (e *Engine) SetTTSConfig(cfg *TTSCfg) {
	e.tts = cfg
}

// SetTTSSaveFunc registers a callback that persists TTS mode changes.
func (e *Engine) SetTTSSaveFunc(fn func(mode string) error) {
	e.ttsSaveFunc = fn
}

// SetDisplayConfig overrides the default truncation settings.
func (e *Engine) SetDisplayConfig(cfg DisplayCfg) {
	e.display = cfg
}

// SetDefaultQuiet sets whether new sessions start in quiet mode.
func (e *Engine) SetDefaultQuiet(q bool) {
	e.defaultQuiet = q
}

// SetInjectSender controls whether sender identity (platform and user ID) is
// prepended to each message before forwarding it to the agent. When enabled,
// the agent receives a preamble line like:
//
//	[cc-connect sender_id=ou_abc123 platform=feishu]
//
// This allows the agent to identify who sent the message and adjust behavior
// accordingly (e.g. personal task views, role-based access control).
func (e *Engine) SetInjectSender(v bool) {
	e.injectSender = v
}

func (e *Engine) SetLanguageSaveFunc(fn func(Language) error) {
	e.i18n.SetSaveFunc(fn)
}

func (e *Engine) SetProviderSaveFunc(fn func(providerName string) error) {
	e.providerSaveFunc = fn
}

func (e *Engine) SetProviderAddSaveFunc(fn func(ProviderConfig) error) {
	e.providerAddSaveFunc = fn
}

func (e *Engine) SetProviderRemoveSaveFunc(fn func(string) error) {
	e.providerRemoveSaveFunc = fn
}

func (e *Engine) SetCronScheduler(cs *CronScheduler) {
	e.cronScheduler = cs
}

func (e *Engine) SetCommandSaveAddFunc(fn func(name, description, prompt, exec, workDir string) error) {
	e.commandSaveAddFunc = fn
}

func (e *Engine) SetCommandSaveDelFunc(fn func(name string) error) {
	e.commandSaveDelFunc = fn
}

func (e *Engine) SetDisplaySaveFunc(fn func(thinkingMaxLen, toolMaxLen *int) error) {
	e.displaySaveFunc = fn
}

// ConfigReloadResult describes what was updated by a config reload.
type ConfigReloadResult struct {
	DisplayUpdated   bool
	ProvidersUpdated int
	CommandsUpdated  int
}

func (e *Engine) SetConfigReloadFunc(fn func() (*ConfigReloadResult, error)) {
	e.configReloadFunc = fn
}

// GetAgent returns the engine's agent (for type assertions like ProviderSwitcher).
func (e *Engine) GetAgent() Agent {
	return e.agent
}

// AddCommand registers a custom slash command.
func (e *Engine) AddCommand(name, description, prompt, exec, workDir, source string) {
	e.commands.Add(name, description, prompt, exec, workDir, source)
}

// ClearCommands removes all commands from the given source.
func (e *Engine) ClearCommands(source string) {
	e.commands.ClearSource(source)
}

// AddAlias registers a command alias.
func (e *Engine) AddAlias(name, command string) {
	e.aliasMu.Lock()
	defer e.aliasMu.Unlock()
	e.aliases[name] = command
}

func (e *Engine) SetAliasSaveAddFunc(fn func(name, command string) error) {
	e.aliasSaveAddFunc = fn
}

func (e *Engine) SetAliasSaveDelFunc(fn func(name string) error) {
	e.aliasSaveDelFunc = fn
}

// ClearAliases removes all aliases (for config reload).
func (e *Engine) ClearAliases() {
	e.aliasMu.Lock()
	defer e.aliasMu.Unlock()
	e.aliases = make(map[string]string)
}

// SetDisabledCommands sets the list of command IDs that are disabled for this project.
func (e *Engine) SetDisabledCommands(cmds []string) {
	m := make(map[string]bool, len(cmds))
	for _, c := range cmds {
		c = strings.ToLower(strings.TrimPrefix(c, "/"))
		// Resolve alias names to canonical IDs
		id := matchPrefix(c, builtinCommands)
		if id != "" {
			m[id] = true
		} else {
			m[c] = true
		}
	}
	e.disabledCmds = m
}

// SetAdminFrom sets the admin allowlist for privileged commands.
// "*" means all users who pass allow_from are admins.
// Empty string means privileged commands are denied for everyone.
func (e *Engine) SetAdminFrom(adminFrom string) {
	e.adminFrom = strings.TrimSpace(adminFrom)
	if e.adminFrom == "" && !e.disabledCmds["shell"] {
		slog.Warn("admin_from is not set — privileged commands (/shell, /restart, /upgrade) are blocked. "+
			"Set admin_from in config to enable them, or use disabled_commands to hide them.",
			"project", e.name)
	}
}

// privilegedCommands are commands that require admin_from authorization.
var privilegedCommands = map[string]bool{
	"shell":   true,
	"restart": true,
	"upgrade": true,
	"approve": true,
	"reject":  true,
}

// isAdmin checks whether the given user ID is authorized for privileged commands.
// Unlike AllowList, empty adminFrom means deny-all (fail-closed).
func (e *Engine) isAdmin(userID string) bool {
	af := strings.TrimSpace(e.adminFrom)
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

// SetBannedWords replaces the banned words list.
func (e *Engine) SetBannedWords(words []string) {
	e.bannedMu.Lock()
	defer e.bannedMu.Unlock()
	lower := make([]string, len(words))
	for i, w := range words {
		lower[i] = strings.ToLower(w)
	}
	e.bannedWords = lower
}

// SetRateLimitCfg configures per-session message rate limiting.
func (e *Engine) SetRateLimitCfg(cfg RateLimitCfg) {
	e.rateLimiter = NewRateLimiter(cfg.MaxMessages, cfg.Window)
}

// SetStreamPreviewCfg configures the streaming preview behavior.
func (e *Engine) SetStreamPreviewCfg(cfg StreamPreviewCfg) {
	e.streamPreview = cfg
}

// SetEventIdleTimeout sets the maximum time to wait between consecutive agent events.
// 0 disables the timeout entirely.
func (e *Engine) SetEventIdleTimeout(d time.Duration) {
	e.eventIdleTimeout = d
}

// SetAgentPool attaches an AgentPool for multi-agent switching.
func (e *Engine) SetAgentPool(pool *AgentPool) {
	e.agentPool = pool
}

func (e *Engine) SetRelayManager(rm *RelayManager) {
	e.relayManager = rm
}

func (e *Engine) RelayManager() *RelayManager {
	return e.relayManager
}

// RemoveCommand removes a custom command by name. Returns false if not found.
func (e *Engine) RemoveCommand(name string) bool {
	return e.commands.Remove(name)
}

func (e *Engine) ProjectName() string {
	return e.name
}

// ExecuteCronJob runs a cron job by injecting a synthetic message into the engine.
// It finds the platform that owns the session key, reconstructs a reply context,
// and processes the message as if the user sent it.
func (e *Engine) ExecuteCronJob(job *CronJob) error {
	sessionKey := job.SessionKey
	platformName := ""
	if idx := strings.Index(sessionKey, ":"); idx > 0 {
		platformName = sessionKey[:idx]
	}

	var targetPlatform Platform
	for _, p := range e.platforms {
		if p.Name() == platformName {
			targetPlatform = p
			break
		}
	}
	if targetPlatform == nil {
		return fmt.Errorf("platform %q not found for session %q", platformName, sessionKey)
	}

	rc, ok := targetPlatform.(ReplyContextReconstructor)
	if !ok {
		return fmt.Errorf("platform %q does not support proactive messaging (cron)", platformName)
	}

	replyCtx, err := rc.ReconstructReplyCtx(sessionKey)
	if err != nil {
		return fmt.Errorf("reconstruct reply context: %w", err)
	}

	// Notify user that a cron job is executing (unless silent)
	silent := false
	if e.cronScheduler != nil {
		silent = e.cronScheduler.IsSilent(job)
	}
	if !silent {
		desc := job.Description
		if desc == "" {
			desc = truncateStr(job.Prompt, 40)
		}
		e.send(targetPlatform, replyCtx, fmt.Sprintf("⏰ %s", desc))
	}

	msg := &Message{
		SessionKey: sessionKey,
		Platform:   platformName,
		UserID:     "cron",
		UserName:   "cron",
		Content:    job.Prompt,
		ReplyCtx:   replyCtx,
	}

	conv := e.conversations.GetOrCreate(sessionKey)
	if !conv.TryLock() {
		return fmt.Errorf("session %q is busy", sessionKey)
	}

	e.processInteractiveMessage(targetPlatform, msg, conv)
	return nil
}

func (e *Engine) Start() error {
	var startErrs []error
	for _, p := range e.platforms {
		if err := p.Start(e.handleMessage); err != nil {
			slog.Warn("platform start failed", "project", e.name, "platform", p.Name(), "error", err)
			startErrs = append(startErrs, fmt.Errorf("[%s] start platform %s: %w", e.name, p.Name(), err))
			continue
		}
		slog.Info("platform started", "project", e.name, "platform", p.Name())

		// Register commands on platforms that support it (e.g. Telegram setMyCommands)
		if registrar, ok := p.(CommandRegistrar); ok {
			commands := e.GetAllCommands()
			if err := registrar.RegisterCommands(commands); err != nil {
				slog.Error("platform command registration failed", "project", e.name, "platform", p.Name(), "error", err)
			} else {
				slog.Debug("platform commands registered", "project", e.name, "platform", p.Name(), "count", len(commands))
			}
		}

		if nav, ok := p.(CardNavigable); ok {
			nav.SetCardNavigationHandler(e.handleCardNav)
		}
	}

	// Log summary
	startedCount := len(e.platforms) - len(startErrs)
	if len(startErrs) > 0 {
		slog.Warn("engine started with some failures", "project", e.name, "agent", e.agent.Name(), "started", startedCount, "failed", len(startErrs))
	} else {
		slog.Info("engine started", "project", e.name, "agent", e.agent.Name(), "platforms", len(e.platforms))
	}

	// Only return error if ALL platforms failed
	if len(startErrs) == len(e.platforms) && len(e.platforms) > 0 {
		return startErrs[0] // Return first error
	}
	return nil
}

func (e *Engine) Stop() error {
	// Stop platforms first to prevent new incoming messages
	var errs []error
	for _, p := range e.platforms {
		if err := p.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("stop platform %s: %w", p.Name(), err))
		}
	}

	// Now cancel context and clean up sessions
	e.cancel()

	for _, conv := range e.conversations.List() {
		conv.mu.Lock()
		as := conv.AgentSession
		conv.AgentSession = nil
		conv.mu.Unlock()
		if as != nil {
			slog.Debug("engine.Stop: closing agent session", "session", conv.Key)
			as.Close()
		}
	}
	e.conversations.Save()

	if err := e.agent.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("stop agent %s: %w", e.agent.Name(), err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("engine stop errors: %v", errs)
	}
	return nil
}

// matchBannedWord returns the first banned word found in content, or "".
func (e *Engine) matchBannedWord(content string) string {
	e.bannedMu.RLock()
	defer e.bannedMu.RUnlock()
	if len(e.bannedWords) == 0 {
		return ""
	}
	lower := strings.ToLower(content)
	for _, w := range e.bannedWords {
		if strings.Contains(lower, w) {
			return w
		}
	}
	return ""
}

// resolveAlias checks if the content (or its first word) matches an alias and replaces it.
func (e *Engine) resolveAlias(content string) string {
	e.aliasMu.RLock()
	defer e.aliasMu.RUnlock()

	if len(e.aliases) == 0 {
		return content
	}

	// Exact match on full content
	if cmd, ok := e.aliases[content]; ok {
		return cmd
	}

	// Match first word, append remaining args
	parts := strings.SplitN(content, " ", 2)
	if cmd, ok := e.aliases[parts[0]]; ok {
		if len(parts) > 1 {
			return cmd + " " + parts[1]
		}
		return cmd
	}
	return content
}

func (e *Engine) handleMessage(p Platform, msg *Message) {
	slog.Info("message received",
		"platform", msg.Platform, "msg_id", msg.MessageID,
		"session", msg.SessionKey, "user", msg.UserName,
		"content_len", len(msg.Content),
		"has_images", len(msg.Images) > 0, "has_audio", msg.Audio != nil, "has_files", len(msg.Files) > 0,
	)

	// Voice message: transcribe to text first
	if msg.Audio != nil {
		e.handleVoiceMessage(p, msg)
		return
	}

	content := strings.TrimSpace(msg.Content)
	if content == "" && len(msg.Images) == 0 && len(msg.Files) == 0 {
		return
	}

	// Resolve aliases: check if the first word (or whole content) matches an alias
	content = e.resolveAlias(content)
	msg.Content = content

	// Rate limit check
	if e.rateLimiter != nil && !e.rateLimiter.Allow(msg.SessionKey) {
		slog.Info("message rate limited", "session", msg.SessionKey, "user", msg.UserName)
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRateLimited))
		return
	}

	// Banned words check (skip for slash commands)
	if !strings.HasPrefix(content, "/") {
		if word := e.matchBannedWord(content); word != "" {
			slog.Info("message blocked by banned word", "word", word, "user", msg.UserName)
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgBannedWordBlocked))
			return
		}
	}

	// Multi-workspace resolution
	var wsAgent Agent
	var resolvedWorkspace string
	if e.multiWorkspace {
		channelID := extractChannelID(msg.SessionKey)
		workspace, channelName, err := e.resolveWorkspace(p, channelID)
		if err != nil {
			slog.Error("workspace resolution failed", "err", err)
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsResolutionError, err))
			return
		}
		if workspace == "" {
			// No workspace — handle init flow (unless it's a /workspace command)
			if !strings.HasPrefix(content, "workspace") && !strings.HasPrefix(content, "ws ") {
				if e.handleWorkspaceInitFlow(p, msg, channelID, channelName) {
					return
				}
			}
			// If init flow didn't consume, only workspace commands work
			if !strings.HasPrefix(content, "/") {
				return
			}
		} else {
			resolvedWorkspace = workspace

			// Touch for idle tracking
			if ws := e.workspacePool.Get(workspace); ws != nil {
				ws.Touch()
			}

			wsAgent, err = e.getOrCreateWorkspaceAgent(workspace)
			if err != nil {
				slog.Error("failed to create workspace agent", "workspace", workspace, "err", err)
				e.reply(p, msg.ReplyCtx, fmt.Sprintf("Failed to initialize workspace: %v", err))
				return
			}
		}
	}

	if len(msg.Images) == 0 && strings.HasPrefix(content, "/") {
		if e.handleCommand(p, msg, content) {
			return
		}
		// Unrecognized slash command — fall through to agent as normal message
	}

	// Permission responses bypass the session lock
	if e.handlePendingPermission(p, msg, content) {
		return
	}

	// Select agent based on workspace mode
	agent := e.agent
	interactiveKey := msg.SessionKey
	if e.multiWorkspace && wsAgent != nil {
		agent = wsAgent
		interactiveKey = resolvedWorkspace + ":" + msg.SessionKey
	}

	// Admin session routing: if admin is "visiting" another user's session,
	// remap the session key so messages go to the target session's Agent.
	// The reply context is kept so responses go back to the admin's chat.
	originalSessionKey := msg.SessionKey
	if target, ok := e.GetAdminOverride(msg.SessionKey); ok {
		slog.Info("admin routing: redirecting to target session",
			"admin", msg.SessionKey, "target", target)
		msg.SessionKey = target
		interactiveKey = target
	}

	// Track session owner
	if _, overridden := e.GetAdminOverride(originalSessionKey); !overridden {
		ownerConv := e.conversations.GetOrCreate(interactiveKey)
		ownerConv.OwnerID = msg.UserID
	}

	conv := e.conversations.GetOrCreate(interactiveKey)
	if !conv.TryLock() {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPreviousProcessing))
		return
	}

	slog.Info("processing message",
		"platform", msg.Platform,
		"user", msg.UserName,
		"session", conv.Key,
	)

	go e.processInteractiveMessageWith(p, msg, conv, agent, interactiveKey, resolvedWorkspace)
}

// ──────────────────────────────────────────────────────────────
// Voice message handling
// ──────────────────────────────────────────────────────────────

func (e *Engine) handleVoiceMessage(p Platform, msg *Message) {
	if !e.speech.Enabled || e.speech.STT == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgVoiceNotEnabled))
		return
	}

	audio := msg.Audio
	if NeedsConversion(audio.Format) && !HasFFmpeg() {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgVoiceNoFFmpeg))
		return
	}

	slog.Info("transcribing voice message",
		"platform", msg.Platform, "user", msg.UserName,
		"format", audio.Format, "size", len(audio.Data),
	)
	e.send(p, msg.ReplyCtx, e.i18n.T(MsgVoiceTranscribing))

	text, err := TranscribeAudio(e.ctx, e.speech.STT, audio, e.speech.Language)
	if err != nil {
		slog.Error("speech transcription failed", "error", err)
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgVoiceTranscribeFailed), err))
		return
	}

	text = strings.TrimSpace(text)
	if text == "" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgVoiceEmpty))
		return
	}

	slog.Info("voice transcribed", "text_len", len(text))
	e.send(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgVoiceTranscribed), text))

	// Replace audio with transcribed text and re-dispatch
	msg.Audio = nil
	msg.Content = text
	msg.FromVoice = true
	e.handleMessage(p, msg)
}

// ──────────────────────────────────────────────────────────────
// Permission handling
// ──────────────────────────────────────────────────────────────

func (e *Engine) handlePendingPermission(p Platform, msg *Message, content string) bool {
	conv := e.conversations.Get(msg.SessionKey)
	if conv == nil && e.multiWorkspace {
		suffix := ":" + msg.SessionKey
		for _, c := range e.conversations.List() {
			if strings.HasSuffix(c.Key, suffix) {
				conv = c
				break
			}
		}
	}
	if conv == nil {
		return false
	}

	conv.mu.Lock()
	pending := conv.PendingPerm
	conv.mu.Unlock()
	if pending == nil {
		return false
	}

	// AskUserQuestion: interpret user response as an answer, not a permission decision
	if len(pending.Questions) > 0 {
		curIdx := pending.CurrentQuestion
		q := pending.Questions[curIdx]
		answer := e.resolveAskQuestionAnswer(q, content)

		if pending.Answers == nil {
			pending.Answers = make(map[int]string)
		}
		pending.Answers[curIdx] = answer

		// More questions remaining — advance to next and send new card
		if curIdx+1 < len(pending.Questions) {
			pending.CurrentQuestion = curIdx + 1
			e.reply(p, msg.ReplyCtx, fmt.Sprintf("✅ %s: **%s**", q.Question, answer))
			e.sendAskQuestionPrompt(p, msg.ReplyCtx, pending.Questions, curIdx+1)
			return true
		}

		// All questions answered — build response and resolve
		updatedInput := buildAskQuestionResponse(pending.ToolInput, pending.Questions, pending.Answers)

		if err := conv.AgentSession.RespondPermission(pending.RequestID, PermissionResult{
			Behavior:     "allow",
			UpdatedInput: updatedInput,
		}); err != nil {
			slog.Error("failed to send AskUserQuestion response", "error", err)
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
		} else {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf("✅ %s: **%s**", q.Question, answer))
		}

		conv.mu.Lock()
		conv.PendingPerm = nil
		conv.mu.Unlock()
		pending.resolve()
		return true
	}

	lower := strings.ToLower(strings.TrimSpace(content))

	if isApproveAllResponse(lower) {
		conv.mu.Lock()
		conv.ApproveAll = true
		conv.mu.Unlock()

		if err := conv.AgentSession.RespondPermission(pending.RequestID, PermissionResult{
			Behavior:     "allow",
			UpdatedInput: pending.ToolInput,
		}); err != nil {
			slog.Error("failed to send permission response", "error", err)
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPermissionApproveAll))
		}
	} else if isAllowResponse(lower) {
		if err := conv.AgentSession.RespondPermission(pending.RequestID, PermissionResult{
			Behavior:     "allow",
			UpdatedInput: pending.ToolInput,
		}); err != nil {
			slog.Error("failed to send permission response", "error", err)
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPermissionAllowed))
		}
	} else if isDenyResponse(lower) {
		if err := conv.AgentSession.RespondPermission(pending.RequestID, PermissionResult{
			Behavior: "deny",
			Message:  "User denied this tool use.",
		}); err != nil {
			slog.Error("failed to send deny response", "error", err)
		}
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPermissionDenied))
	} else {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPermissionHint))
		return true
	}

	conv.mu.Lock()
	conv.PendingPerm = nil
	conv.mu.Unlock()
	pending.resolve()

	return true
}

// resolveAskQuestionAnswer converts user input into answer text.
// It handles button callbacks ("askq:qIdx:optIdx"), numeric selections ("1", "1,3"), and free text.
func (e *Engine) resolveAskQuestionAnswer(q UserQuestion, input string) string {
	input = strings.TrimSpace(input)

	// Handle card button callback: "askq:qIdx:optIdx"
	if strings.HasPrefix(input, "askq:") {
		parts := strings.SplitN(input, ":", 3)
		if len(parts) == 3 {
			if idx, err := strconv.Atoi(parts[2]); err == nil && idx >= 1 && idx <= len(q.Options) {
				return q.Options[idx-1].Label
			}
		}
		// Legacy format "askq:N"
		if len(parts) == 2 {
			if idx, err := strconv.Atoi(parts[1]); err == nil && idx >= 1 && idx <= len(q.Options) {
				return q.Options[idx-1].Label
			}
		}
	}

	// Try numeric index(es)
	if q.MultiSelect {
		parts := strings.FieldsFunc(input, func(r rune) bool { return r == ',' || r == '，' || r == ' ' })
		var labels []string
		allNumeric := true
		for _, p := range parts {
			p = strings.TrimSpace(p)
			idx, err := strconv.Atoi(p)
			if err != nil || idx < 1 || idx > len(q.Options) {
				allNumeric = false
				break
			}
			labels = append(labels, q.Options[idx-1].Label)
		}
		if allNumeric && len(labels) > 0 {
			return strings.Join(labels, ", ")
		}
	} else {
		if idx, err := strconv.Atoi(input); err == nil && idx >= 1 && idx <= len(q.Options) {
			return q.Options[idx-1].Label
		}
	}

	return input
}

// buildAskQuestionResponse constructs the updatedInput for AskUserQuestion control_response.
func buildAskQuestionResponse(originalInput map[string]any, questions []UserQuestion, collected map[int]string) map[string]any {
	result := make(map[string]any)
	for k, v := range originalInput {
		result[k] = v
	}
	answers := make(map[string]any)
	for idx, ans := range collected {
		answers[strconv.Itoa(idx)] = ans
	}
	result["answers"] = answers
	return result
}

func isApproveAllResponse(s string) bool {
	for _, w := range []string{
		"allow all", "allowall", "approve all", "yes all",
		"允许所有", "允许全部", "全部允许", "所有允许", "都允许", "全部同意",
	} {
		if s == w {
			return true
		}
	}
	return false
}

func isAllowResponse(s string) bool {
	for _, w := range []string{"allow", "yes", "y", "ok", "允许", "同意", "可以", "好", "好的", "是", "确认", "approve"} {
		if s == w {
			return true
		}
	}
	return false
}

func isDenyResponse(s string) bool {
	for _, w := range []string{"deny", "no", "n", "reject", "拒绝", "不允许", "不行", "不", "否", "取消", "cancel"} {
		if s == w {
			return true
		}
	}
	return false
}

// ──────────────────────────────────────────────────────────────
// Interactive agent processing
// ──────────────────────────────────────────────────────────────

func (e *Engine) processInteractiveMessage(p Platform, msg *Message, conv *ConversationContext) {
	e.processInteractiveMessageWith(p, msg, conv, e.agent, msg.SessionKey, "")
}

// processInteractiveMessageWith is the core interactive processing loop.
// It accepts an explicit agent, interactiveKey (for the conversations map),
// and workspaceDir so that multi-workspace mode can route to per-workspace agents.
func (e *Engine) processInteractiveMessageWith(p Platform, msg *Message, conv *ConversationContext, agent Agent, interactiveKey string, workspaceDir string) {
	defer conv.Unlock()

	if e.ctx.Err() != nil {
		return
	}

	turnStart := time.Now()

	e.i18n.DetectAndSet(msg.Content)
	conv.AddHistory("user", msg.Content)

	var agentOverride Agent
	if agent != e.agent {
		agentOverride = agent
	}
	conv = e.getOrCreateConversation(interactiveKey, p, msg.ReplyCtx, agentOverride)

	if workspaceDir != "" {
		conv.mu.Lock()
		conv.WorkspaceDir = workspaceDir
		conv.mu.Unlock()
	}

	conv.mu.Lock()
	conv.ReplyPlatform = p
	conv.ReplyCtx = msg.ReplyCtx
	conv.mu.Unlock()

	if conv.AgentSession == nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), "failed to start agent session"))
		return
	}

	// Block user messages while plan is under admin review.
	// This must be checked BEFORE sending the message to the Agent,
	// because the Agent's own permission mode may auto-approve operations,
	// bypassing cc-connect's EventPermissionRequest-level gating.
	if e.adminFrom != "" {
		conv.mu.Lock()
		phase := conv.ApprovalPhase
		pendingUserMsg := conv.UserMessage
		pendingPlan := conv.PlanText
		conv.mu.Unlock()
		if phase == PhasePending {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgApprovalPendingUser))
			// Re-send approval card to admin in case they missed it
			go e.sendApprovalCardToAdmin(p, interactiveKey, pendingUserMsg, pendingPlan)
			return
		}
	}

	var stopTyping func()
	if ti, ok := p.(TypingIndicator); ok {
		stopTyping = ti.StartTyping(e.ctx, msg.ReplyCtx)
	}
	defer func() {
		if stopTyping != nil {
			stopTyping()
		}
	}()

	drainEvents(conv.AgentSession.Events())

	promptContent := msg.Content
	if e.injectSender && msg.UserID != "" {
		chatID := extractChannelID(msg.SessionKey)
		promptContent = fmt.Sprintf("[cc-connect sender_id=%s platform=%s chat_id=%s]\n%s", msg.UserID, msg.Platform, chatID, msg.Content)
	}

	sendStart := time.Now()
	conv.mu.Lock()
	conv.FromVoice = msg.FromVoice
	conv.mu.Unlock()
	if err := conv.AgentSession.Send(promptContent, msg.Images, msg.Files); err != nil {
		slog.Error("failed to send prompt", "error", err)

		if !conv.AgentSession.Alive() {
			e.cleanupConversation(interactiveKey, conv.AgentSession)
			e.send(p, msg.ReplyCtx, e.i18n.T(MsgSessionRestarting))

			conv = e.getOrCreateConversation(interactiveKey, p, msg.ReplyCtx, agentOverride)
			if workspaceDir != "" {
				conv.mu.Lock()
				conv.WorkspaceDir = workspaceDir
				conv.mu.Unlock()
			}
			if conv.AgentSession == nil {
				e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), "failed to restart agent session"))
				return
			}
			sendStart = time.Now()
			if err := conv.AgentSession.Send(promptContent, msg.Images, msg.Files); err != nil {
				e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
				return
			}
		} else {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
			return
		}
	}
	if elapsed := time.Since(sendStart); elapsed >= slowAgentSend {
		slog.Warn("slow agent send", "elapsed", elapsed, "session", msg.SessionKey, "content_len", len(msg.Content))
	}

	e.processInteractiveEvents(conv, interactiveKey, msg.MessageID, turnStart)
}

// getOrCreateWorkspaceAgent returns (or creates) a per-workspace agent.
// ConversationStore handles per-conversation state with workspace-prefixed keys.
func (e *Engine) getOrCreateWorkspaceAgent(workspace string) (Agent, error) {
	ws := e.workspacePool.GetOrCreate(workspace)
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if ws.agent != nil {
		return ws.agent, nil
	}

	opts := make(map[string]any)
	opts["work_dir"] = workspace

	if ma, ok := e.agent.(interface{ GetModel() string }); ok {
		if m := ma.GetModel(); m != "" {
			opts["model"] = m
		}
	}
	if ma, ok := e.agent.(interface{ GetMode() string }); ok {
		if m := ma.GetMode(); m != "" {
			opts["mode"] = m
		}
	}

	agent, err := CreateAgent(e.agent.Name(), opts)
	if err != nil {
		return nil, fmt.Errorf("create workspace agent for %s: %w", workspace, err)
	}

	if ps, ok := e.agent.(ProviderSwitcher); ok {
		if ps2, ok2 := agent.(ProviderSwitcher); ok2 {
			ps2.SetProviders(ps.ListProviders())
		}
	}

	ws.agent = agent
	return agent, nil
}

// getOrCreateConversation returns or creates the ConversationContext for the given key,
// starting a new agent session if needed. This is the ConversationStore-based replacement
// for getOrCreateInteractiveStateWith.
func (e *Engine) getOrCreateConversation(sessionKey string, p Platform, replyCtx any, agentOverride Agent) *ConversationContext {
	conv := e.conversations.GetOrCreate(sessionKey)

	conv.mu.Lock()
	if conv.AgentSession != nil && conv.AgentSession.Alive() {
		wantID := conv.AgentSessionID
		currentID := conv.AgentSession.CurrentSessionID()
		if wantID == "" || currentID == "" || wantID == currentID {
			conv.ReplyPlatform = p
			conv.ReplyCtx = replyCtx
			conv.mu.Unlock()
			return conv
		}
		slog.Info("interactive session mismatch, recycling",
			"session_key", sessionKey,
			"want_agent_session", wantID,
			"have_agent_session", currentID,
		)
		oldSession := conv.AgentSession
		conv.AgentSession = nil
		conv.mu.Unlock()
		go oldSession.Close()
	} else {
		conv.mu.Unlock()
	}

	quietMode := e.defaultQuiet
	conv.mu.Lock()
	if conv.Quiet {
		quietMode = conv.Quiet
	}
	conv.mu.Unlock()

	agent := e.agent
	if agentOverride != nil {
		agent = agentOverride
	}

	if inj, ok := agent.(SessionEnvInjector); ok {
		envVars := []string{
			"CC_PROJECT=" + e.name,
			"CC_SESSION_KEY=" + sessionKey,
		}
		if exePath, err := os.Executable(); err == nil {
			binDir := filepath.Dir(exePath)
			if curPath := os.Getenv("PATH"); curPath != "" {
				envVars = append(envVars, "PATH="+binDir+string(filepath.ListSeparator)+curPath)
			} else {
				envVars = append(envVars, "PATH="+binDir)
			}
		}
		inj.SetSessionEnv(envVars)
	}

	if e.ctx.Err() != nil {
		slog.Debug("skipping session start: context canceled", "session_key", sessionKey)
		conv.mu.Lock()
		conv.ReplyPlatform = p
		conv.ReplyCtx = replyCtx
		conv.Quiet = quietMode
		conv.mu.Unlock()
		return conv
	}

	startAt := time.Now()
	agentSession, err := agent.StartSession(e.ctx, conv.AgentSessionID)
	startElapsed := time.Since(startAt)
	if err != nil {
		slog.Error("failed to start interactive session", "error", err, "elapsed", startElapsed)
		conv.mu.Lock()
		conv.ReplyPlatform = p
		conv.ReplyCtx = replyCtx
		conv.Quiet = quietMode
		conv.mu.Unlock()
		return conv
	}
	if startElapsed >= slowAgentStart {
		slog.Warn("slow agent session start", "elapsed", startElapsed, "agent", agent.Name(), "session_id", conv.AgentSessionID)
	}

	if newID := agentSession.CurrentSessionID(); newID != "" {
		conv.CompareAndSetAgentSessionID(newID)
	}

	conv.mu.Lock()
	conv.AgentSession = agentSession
	conv.ReplyPlatform = p
	conv.ReplyCtx = replyCtx
	conv.Quiet = quietMode
	conv.mu.Unlock()

	slog.Info("interactive session started", "session_key", sessionKey, "agent_session", conv.AgentSessionID, "elapsed", startElapsed)
	return conv
}

// cleanupConversation clears runtime state and closes the agent session.
// When expectedSession is non-nil, cleanup is skipped if the conversation's
// agent session has been replaced (prevents stale goroutines from killing new sessions).
func (e *Engine) cleanupConversation(sessionKey string, expectedSession ...AgentSession) {
	conv := e.conversations.Get(sessionKey)
	if conv == nil {
		return
	}

	conv.mu.Lock()
	if len(expectedSession) > 0 && expectedSession[0] != nil && conv.AgentSession != expectedSession[0] {
		conv.mu.Unlock()
		return
	}
	agentSession := conv.AgentSession
	conv.AgentSession = nil
	conv.ReplyPlatform = nil
	conv.ReplyCtx = nil
	conv.PendingPerm = nil
	conv.DeleteMode = nil
	conv.mu.Unlock()

	if agentSession != nil {
		slog.Debug("cleanupConversation: closing agent session", "session", sessionKey)
		closeStart := time.Now()

		done := make(chan struct{})
		go func() {
			agentSession.Close()
			close(done)
		}()

		select {
		case <-done:
			if elapsed := time.Since(closeStart); elapsed >= slowAgentClose {
				slog.Warn("slow agent session close", "elapsed", elapsed, "session", sessionKey)
			}
		case <-time.After(10 * time.Second):
			slog.Error("agent session close timed out (10s), abandoning", "session", sessionKey)
		}
	}
}

const defaultEventIdleTimeout = 2 * time.Hour

func (e *Engine) processInteractiveEvents(conv *ConversationContext, sessionKey string, msgID string, turnStart time.Time) {
	var textParts []string
	toolCount := 0
	waitStart := time.Now()
	firstEventLogged := false

	conv.mu.Lock()
	sp := newStreamPreview(e.streamPreview, conv.ReplyPlatform, conv.ReplyCtx, e.ctx)
	conv.mu.Unlock()

	// Idle timeout: 0 = disabled
	var idleTimer *time.Timer
	var idleCh <-chan time.Time
	if e.eventIdleTimeout > 0 {
		idleTimer = time.NewTimer(e.eventIdleTimeout)
		defer idleTimer.Stop()
		idleCh = idleTimer.C
	}

	events := conv.AgentSession.Events()
	for {
		var event Event
		var ok bool

		select {
		case event, ok = <-events:
			if !ok {
				goto channelClosed
			}
		case <-idleCh:
			slog.Error("agent session idle timeout: no events for too long, killing session",
				"session_key", sessionKey, "timeout", e.eventIdleTimeout, "elapsed", time.Since(turnStart))
			sp.finish("")
			conv.mu.Lock()
			p := conv.ReplyPlatform
			replyCtx := conv.ReplyCtx
			conv.mu.Unlock()
			e.send(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgError), "agent session timed out (no response)"))
			e.cleanupConversation(sessionKey, conv.AgentSession)
			return
		case <-e.ctx.Done():
			return
		}

		// Reset idle timer after receiving an event
		if idleTimer != nil {
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(e.eventIdleTimeout)
		}

		if !firstEventLogged {
			firstEventLogged = true
			if elapsed := time.Since(waitStart); elapsed >= slowAgentFirstEvent {
				slog.Warn("slow agent first event", "elapsed", elapsed, "session", sessionKey, "event_type", event.Type)
			}
		}

		conv.mu.Lock()
		p := conv.ReplyPlatform
		replyCtx := conv.ReplyCtx
		sessionQuiet := conv.Quiet
		conv.mu.Unlock()

		e.quietMu.RLock()
		globalQuiet := e.quiet
		e.quietMu.RUnlock()

		quiet := globalQuiet || sessionQuiet

		switch event.Type {
		case EventThinking:
			if !quiet && event.Content != "" {
				sp.freeze()
				preview := truncateIf(event.Content, e.display.ThinkingMaxLen)
				e.send(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgThinking), preview))
			}

		case EventToolUse:
			toolCount++
			if !quiet {
				sp.freeze()
				inputPreview := truncateIf(event.ToolInput, e.display.ToolMaxLen)
				// Use code block if content is long (>5 lines or >200 chars), otherwise inline code
				lineCount := strings.Count(inputPreview, "\n") + 1
				var formattedInput string
				if lineCount > 5 || utf8.RuneCountInString(inputPreview) > 200 {
					formattedInput = fmt.Sprintf("```\n%s\n```", inputPreview)
				} else {
					formattedInput = fmt.Sprintf("`%s`", inputPreview)
				}
				e.send(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgTool), toolCount, event.ToolName, formattedInput))
			}

		case EventText:
			if event.Content != "" {
				textParts = append(textParts, event.Content)
				if sp.canPreview() {
					sp.appendText(event.Content)
				}
			}
			if event.SessionID != "" {
				if conv.CompareAndSetAgentSessionID(event.SessionID) {
					e.conversations.Save()
				}
			}

		case EventPermissionRequest:
			isAskQuestion := event.ToolName == "AskUserQuestion" && len(event.Questions) > 0

			// Phase-based approval: deny tool requests unless in Executing phase
			if e.adminFrom != "" && !isAskQuestion {
				phase := conv.ApprovalPhase
				switch phase {
				case PhasePlanning:
					slog.Info("approval: denying tool in planning phase",
						"session", sessionKey, "tool", event.ToolName)
					_ = conv.AgentSession.RespondPermission(event.RequestID, PermissionResult{
						Behavior: "deny",
					})
					_ = conv.AgentSession.Send(e.i18n.T(MsgApprovalDenyPlanning), nil, nil)
					sp.finish("")
					return
				case PhasePending:
					slog.Info("approval: denying tool in pending phase",
						"session", sessionKey, "tool", event.ToolName)
					_ = conv.AgentSession.RespondPermission(event.RequestID, PermissionResult{
						Behavior: "deny",
					})
					_ = conv.AgentSession.Send(e.i18n.T(MsgApprovalPending), nil, nil)
					sp.finish("")
					return
				}
			}

			conv.mu.Lock()
			autoApprove := conv.ApproveAll
			conv.mu.Unlock()

			if !autoApprove && conv.ApprovalPhase == PhaseExecuting {
				autoApprove = true
			}

			if autoApprove && !isAskQuestion {
				slog.Debug("auto-approving (approve-all)", "request_id", event.RequestID, "tool", event.ToolName)
				_ = conv.AgentSession.RespondPermission(event.RequestID, PermissionResult{
					Behavior:     "allow",
					UpdatedInput: event.ToolInputRaw,
				})
				continue
			}

			// Stop streaming preview before sending prompt
			sp.freeze()

			slog.Info("permission request",
				"request_id", event.RequestID,
				"tool", event.ToolName,
			)

			if isAskQuestion {
				e.sendAskQuestionPrompt(p, replyCtx, event.Questions, 0)
			} else {
				permLimit := e.display.ToolMaxLen
				if permLimit > 0 {
					permLimit = permLimit * 8 / 5
				}
				toolInput := truncateIf(event.ToolInput, permLimit)
				prompt := fmt.Sprintf(e.i18n.T(MsgPermissionPrompt), event.ToolName, toolInput)
				e.sendPermissionPrompt(p, replyCtx, prompt, event.ToolName, toolInput)
			}

			pending := &pendingPermission{
				RequestID:    event.RequestID,
				ToolName:     event.ToolName,
				ToolInput:    event.ToolInputRaw,
				InputPreview: event.ToolInput,
				Questions:    event.Questions,
				Resolved:     make(chan struct{}),
			}
			conv.mu.Lock()
			conv.PendingPerm = pending
			conv.mu.Unlock()

			// Stop idle timer while waiting for user permission response;
			// the user may take a long time to decide, and we don't want
			// the idle timeout to kill the session during that wait.
			if idleTimer != nil {
				idleTimer.Stop()
			}

			<-pending.Resolved
			slog.Info("permission resolved", "request_id", event.RequestID)

			// Restart idle timer after permission is resolved
			if idleTimer != nil {
				idleTimer.Reset(e.eventIdleTimeout)
			}

		case EventResult:
			if event.SessionID != "" {
				conv.SetAgentSessionID(event.SessionID)
			}

			fullResponse := event.Content
			if fullResponse == "" && len(textParts) > 0 {
				fullResponse = strings.Join(textParts, "")
			}
			if fullResponse == "" {
				fullResponse = e.i18n.T(MsgEmptyResponse)
			}

			conv.AddHistory("assistant", fullResponse)
			e.conversations.Save()

			turnDuration := time.Since(turnStart)
			slog.Info("turn complete",
				"session", conv.Key,
				"agent_session", conv.AgentSessionID,
				"msg_id", msgID,
				"tools", toolCount,
				"response_len", len(fullResponse),
				"turn_duration", turnDuration,
			)

			if toolCount > 0 && conv.ApprovalPhase == PhaseExecuting {
				conv.CompleteExecution()
				e.NotifySessionCompletion(sessionKey, truncateStr(fullResponse, 200))
			}


			replyStart := time.Now()

			// If streaming preview was active, try to finalize in-place
			if sp.finish(fullResponse) {
				slog.Debug("EventResult: finalized via stream preview", "response_len", len(fullResponse))
			} else {
				slog.Debug("EventResult: sending via p.Send (preview inactive or failed)", "response_len", len(fullResponse), "chunks", len(splitMessage(fullResponse, maxPlatformMessageLen)))
				for _, chunk := range splitMessage(fullResponse, maxPlatformMessageLen) {
					if err := p.Send(e.ctx, replyCtx, chunk); err != nil {
						slog.Error("failed to send reply", "error", err, "msg_id", msgID)
						return
					}
				}
			}

			if elapsed := time.Since(replyStart); elapsed >= slowPlatformSend {
				slog.Warn("slow final reply send", "platform", p.Name(), "elapsed", elapsed, "response_len", len(fullResponse))
			}

			// TTS: async voice reply if enabled
			if e.tts != nil && e.tts.Enabled && e.tts.TTS != nil {
				conv.mu.Lock()
				fromVoice := conv.FromVoice
				conv.mu.Unlock()
				mode := e.tts.GetTTSMode()
				if mode == "always" || (mode == "voice_only" && fromVoice) {
					go e.sendTTSReply(p, replyCtx, fullResponse)
				}
			}

			return

		case EventError:
			sp.finish("") // clean up preview on error
			if event.Error != nil {
				slog.Error("agent error", "error", event.Error)
				e.send(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgError), event.Error))
			}
			return
		}
	}

channelClosed:
	slog.Warn("agent process exited", "session_key", sessionKey)
	e.cleanupConversation(sessionKey, conv.AgentSession)

	if len(textParts) > 0 {
		conv.mu.Lock()
		p := conv.ReplyPlatform
		replyCtx := conv.ReplyCtx
		conv.mu.Unlock()

		fullResponse := strings.Join(textParts, "")
		conv.AddHistory("assistant", fullResponse)

		if sp.finish(fullResponse) {
			slog.Debug("stream preview: finalized in-place (process exited)")
		} else {
			for _, chunk := range splitMessage(fullResponse, maxPlatformMessageLen) {
				e.send(p, replyCtx, chunk)
			}
		}
	}
}

// ──────────────────────────────────────────────────────────────
// Command handling
// ──────────────────────────────────────────────────────────────

// builtinCommands maps canonical command names to their aliases/full names.
// The first entry is the canonical name used for prefix matching.
var builtinCommands = []struct {
	names []string
	id    string
}{
	{[]string{"new"}, "new"},
	{[]string{"list", "sessions"}, "list"},
	{[]string{"switch"}, "switch"},
	{[]string{"name", "rename"}, "name"},
	{[]string{"current"}, "current"},
	{[]string{"status"}, "status"},
	{[]string{"usage", "quota"}, "usage"},
	{[]string{"history"}, "history"},
	{[]string{"allow"}, "allow"},
	{[]string{"model"}, "model"},
	{[]string{"reasoning", "effort"}, "reasoning"},
	{[]string{"mode"}, "mode"},
	{[]string{"lang"}, "lang"},
	{[]string{"quiet"}, "quiet"},
	{[]string{"provider"}, "provider"},
	{[]string{"memory"}, "memory"},
	{[]string{"cron"}, "cron"},
	{[]string{"compress", "compact"}, "compress"},
	{[]string{"stop"}, "stop"},
	{[]string{"help"}, "help"},
	{[]string{"version"}, "version"},
	{[]string{"commands", "command", "cmd"}, "commands"},
	{[]string{"skills", "skill"}, "skills"},
	{[]string{"config"}, "config"},
	{[]string{"doctor"}, "doctor"},
	{[]string{"upgrade", "update"}, "upgrade"},
	{[]string{"restart"}, "restart"},
	{[]string{"alias"}, "alias"},
	{[]string{"delete", "del", "rm"}, "delete"},
	{[]string{"bind"}, "bind"},
	{[]string{"search", "find"}, "search"},
	{[]string{"shell", "sh", "exec", "run"}, "shell"},
	{[]string{"tts"}, "tts"},
	{[]string{"workspace", "ws"}, "workspace"},
	{[]string{"approve"}, "approve"},
	{[]string{"reject"}, "reject"},
	{[]string{"leave"}, "leave"},
	{[]string{"agent", "engine"}, "agent"},
}

// matchPrefix finds a unique command matching the given prefix.
// Returns the command id or "" if no match / ambiguous.
func matchPrefix(prefix string, candidates []struct {
	names []string
	id    string
}) string {
	// Exact match first
	for _, c := range candidates {
		for _, n := range c.names {
			if prefix == n {
				return c.id
			}
		}
	}
	// Prefix match
	var matched string
	for _, c := range candidates {
		for _, n := range c.names {
			if strings.HasPrefix(n, prefix) {
				if matched != "" && matched != c.id {
					return "" // ambiguous
				}
				matched = c.id
				break
			}
		}
	}
	return matched
}

// matchSubCommand does prefix matching against a flat list of subcommand names.
func matchSubCommand(input string, candidates []string) string {
	for _, c := range candidates {
		if input == c {
			return c
		}
	}
	var matched string
	for _, c := range candidates {
		if strings.HasPrefix(c, input) {
			if matched != "" {
				return input // ambiguous → return raw input (will hit default)
			}
			matched = c
		}
	}
	if matched != "" {
		return matched
	}
	return input
}

func (e *Engine) handleCommand(p Platform, msg *Message, raw string) bool {
	parts := strings.Fields(raw)
	cmd := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
	args := parts[1:]

	cmdID := matchPrefix(cmd, builtinCommands)

	if cmdID != "" && e.disabledCmds[cmdID] {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCommandDisabled), "/"+cmdID))
		return true
	}

	if cmdID != "" && privilegedCommands[cmdID] && !e.isAdmin(msg.UserID) {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgAdminRequired), "/"+cmdID))
		return true
	}

	switch cmdID {
	case "new":
		e.cmdNew(p, msg, args)
	case "list":
		e.cmdList(p, msg, args)
	case "switch":
		e.cmdSwitch(p, msg, args)
	case "name":
		e.cmdName(p, msg, args)
	case "current":
		e.cmdCurrent(p, msg)
	case "status":
		e.cmdStatus(p, msg)
	case "usage":
		e.cmdUsage(p, msg)
	case "history":
		e.cmdHistory(p, msg, args)
	case "allow":
		e.cmdAllow(p, msg, args)
	case "model":
		e.cmdModel(p, msg, args)
	case "reasoning":
		e.cmdReasoning(p, msg, args)
	case "mode":
		e.cmdMode(p, msg, args)
	case "lang":
		e.cmdLang(p, msg, args)
	case "quiet":
		e.cmdQuiet(p, msg, args)
	case "provider":
		e.cmdProvider(p, msg, args)
	case "memory":
		e.cmdMemory(p, msg, args)
	case "cron":
		e.cmdCron(p, msg, args)
	case "compress":
		e.cmdCompress(p, msg)
	case "stop":
		e.cmdStop(p, msg)
	case "help":
		e.cmdHelp(p, msg)
	case "version":
		e.reply(p, msg.ReplyCtx, VersionInfo)
	case "commands":
		e.cmdCommands(p, msg, args)
	case "skills":
		e.cmdSkills(p, msg)
	case "config":
		e.cmdConfig(p, msg, args)
	case "doctor":
		e.cmdDoctor(p, msg)
	case "upgrade":
		e.cmdUpgrade(p, msg, args)
	case "restart":
		e.cmdRestart(p, msg)
	case "alias":
		e.cmdAlias(p, msg, args)
	case "delete":
		e.cmdDelete(p, msg, args)
	case "bind":
		e.cmdBind(p, msg, args)
	case "search":
		e.cmdSearch(p, msg, args)
	case "shell":
		e.cmdShell(p, msg, raw)
	case "tts":
		e.cmdTTS(p, msg, args)
	case "workspace":
		if !e.multiWorkspace {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgWsNotEnabled))
			return true
		}
		e.handleWorkspaceCommand(p, msg, args)
		return true
	case "approve":
		e.cmdApprove(p, msg, args)
	case "reject":
		e.cmdReject(p, msg, args)
	case "leave":
		e.cmdLeave(p, msg)
	case "agent":
		e.cmdAgent(p, msg, args)
	default:
		if custom, ok := e.commands.Resolve(cmd); ok {
			e.executeCustomCommand(p, msg, custom, args)
			return true
		}
		if skill := e.skills.Resolve(cmd); skill != nil {
			e.executeSkill(p, msg, skill, args)
			return true
		}
		// Not a cc-connect command — notify user, then fall through to agent
		e.send(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgUnknownCommand), "/"+cmd))
		return false
	}
	return true
}

func (e *Engine) handleWorkspaceCommand(p Platform, msg *Message, args []string) {
	channelID := extractChannelID(msg.SessionKey)
	projectKey := "project:" + e.name

	subCmd := ""
	if len(args) > 0 {
		subCmd = matchSubCommand(args[0], []string{"init", "bind", "unbind", "list"})
	}

	switch subCmd {
	case "":
		b := e.workspaceBindings.Lookup(projectKey, channelID)
		if b == nil {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgWsNoBinding))
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsInfo, b.Workspace, b.BoundAt.Format(time.RFC3339)))
		}

	case "bind":
		if len(args) < 2 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgWsBindUsage))
			return
		}
		wsName := args[1]
		wsPath := filepath.Join(e.baseDir, wsName)

		// Check if workspace directory exists
		if _, err := os.Stat(wsPath); os.IsNotExist(err) {
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsBindNotFound, wsName))
			return
		}

		channelName := ""
		if resolver, ok := p.(ChannelNameResolver); ok {
			channelName, _ = resolver.ResolveChannelName(channelID)
		}
		e.workspaceBindings.Bind(projectKey, channelID, channelName, wsPath)
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsBindSuccess, wsName))

	case "init":
		if len(args) < 2 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgWsInitUsage))
			return
		}
		repoURL := args[1]
		if !looksLikeGitURL(repoURL) {
			e.reply(p, msg.ReplyCtx, "That doesn't look like a git URL.")
			return
		}

		repoName := extractRepoName(repoURL)
		cloneTo := filepath.Join(e.baseDir, repoName)

		if _, err := os.Stat(cloneTo); err == nil {
			channelName := ""
			if resolver, ok := p.(ChannelNameResolver); ok {
				channelName, _ = resolver.ResolveChannelName(channelID)
			}
			e.workspaceBindings.Bind(projectKey, channelID, channelName, cloneTo)
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsCloneSuccess, cloneTo))
			return
		}

		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsCloneProgress, repoURL))

		if err := gitClone(repoURL, cloneTo); err != nil {
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsCloneFailed, err))
			return
		}

		channelName := ""
		if resolver, ok := p.(ChannelNameResolver); ok {
			channelName, _ = resolver.ResolveChannelName(channelID)
		}
		e.workspaceBindings.Bind(projectKey, channelID, channelName, cloneTo)
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsCloneSuccess, cloneTo))

	case "unbind":
		e.workspaceBindings.Unbind(projectKey, channelID)
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgWsUnbindSuccess))

	case "list":
		bindings := e.workspaceBindings.ListByProject(projectKey)
		if len(bindings) == 0 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgWsListEmpty))
			return
		}
		var sb strings.Builder
		sb.WriteString(e.i18n.T(MsgWsListTitle) + "\n")
		for chID, b := range bindings {
			name := b.ChannelName
			if name == "" {
				name = chID
			}
			sb.WriteString(fmt.Sprintf("• #%s → `%s`\n", name, b.Workspace))
		}
		e.reply(p, msg.ReplyCtx, sb.String())

	default:
		e.reply(p, msg.ReplyCtx,
			"Usage: `/workspace [bind <name> | init <url> | unbind | list]`")
	}
}

func (e *Engine) cmdNew(p Platform, msg *Message, args []string) {
	_, interactiveKey, err := e.commandContext(p, msg)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsResolutionError, err))
		return
	}

	slog.Info("cmdNew: cleaning up old session", "session_key", msg.SessionKey)
	e.cleanupConversation(interactiveKey)
	slog.Info("cmdNew: cleanup done, creating new session", "session_key", msg.SessionKey)
	name := ""
	if len(args) > 0 {
		name = strings.Join(args, " ")
	}
	conv := e.conversations.GetOrCreate(interactiveKey)
	conv.NewSession(name)
	e.conversations.Save()
	if name != "" {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgNewSessionCreatedName), name))
	} else {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNewSessionCreated))
	}
}

const listPageSize = 20

func (e *Engine) cmdList(p Platform, msg *Message, args []string) {
	// Admin mode: show all sessions from all users
	if e.isAdmin(msg.UserID) {
		allConvs := e.conversations.List()
		if len(allConvs) == 0 {
			e.reply(p, msg.ReplyCtx, "📋 暂无任何用户的会话。")
			return
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("📋 **所有会话** (%d 个)\n\n", len(allConvs)))
		for i, conv := range allConvs {
			phaseIcon := ""
			switch conv.ApprovalPhase {
			case PhasePlanning:
				phaseIcon = " 📝"
			case PhasePending:
				phaseIcon = " ⏳"
			case PhaseExecuting:
				phaseIcon = " ✅"
			}
			marker := "◻"
			if target, ok := e.GetAdminOverride(msg.SessionKey); ok && target == conv.Key {
				marker = "▶"
			}
			if conv.Key == msg.SessionKey {
				marker = "▶"
			}
			name := conv.Name
			if name == "" || name == "default" {
				name = conv.Key
			}
			sb.WriteString(fmt.Sprintf("%s **%d.** [%s] %s%s\n", marker, i+1, conv.OwnerID, name, phaseIcon))
		}
		sb.WriteString("\n使用 `/switch <编号>` 进入会话")
		e.reply(p, msg.ReplyCtx, sb.String())
		return
	}

	agent, _, err := e.commandContext(p, msg)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsResolutionError, err))
		return
	}

	if !supportsCards(p) {
		agentSessions, err := agent.ListSessions(e.ctx)
		if err != nil {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgListError), err))
			return
		}
		if len(agentSessions) == 0 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgListEmpty))
			return
		}

		total := len(agentSessions)
		totalPages := (total + listPageSize - 1) / listPageSize

		page := 1
		if len(args) > 0 {
			if n, err := strconv.Atoi(args[0]); err == nil && n > 0 {
				page = n
			}
		}
		if page > totalPages {
			page = totalPages
		}

		start := (page - 1) * listPageSize
		end := start + listPageSize
		if end > total {
			end = total
		}

		agentName := agent.Name()
		activeConv := e.conversations.GetOrCreate(msg.SessionKey)
		activeAgentID := activeConv.AgentSessionID

		var sb strings.Builder
		if totalPages > 1 {
			sb.WriteString(fmt.Sprintf(e.i18n.T(MsgListTitlePaged), agentName, total, page, totalPages))
		} else {
			sb.WriteString(fmt.Sprintf(e.i18n.T(MsgListTitle), agentName, total))
		}
		for i := start; i < end; i++ {
			s := agentSessions[i]
			marker := "◻"
			if s.ID == activeAgentID {
				marker = "▶"
			}
			displayName := e.conversations.GetSessionName(s.ID)
			if displayName != "" {
				displayName = "📌 " + displayName
			} else {
				displayName = strings.ReplaceAll(s.Summary, "\n", " ")
				displayName = strings.Join(strings.Fields(displayName), " ")
				if displayName == "" {
					displayName = "(empty)"
				}
				if len([]rune(displayName)) > 40 {
					displayName = string([]rune(displayName)[:40]) + "…"
				}
			}
			sb.WriteString(fmt.Sprintf("%s **%d.** %s · **%d** msgs · %s\n",
				marker, i+1, displayName, s.MessageCount, s.ModifiedAt.Format("01-02 15:04")))
		}
		if totalPages > 1 {
			sb.WriteString(fmt.Sprintf(e.i18n.T(MsgListPageHint), page, totalPages))
		}
		sb.WriteString(e.i18n.T(MsgListSwitchHint))
		e.reply(p, msg.ReplyCtx, sb.String())
		return
	}

	page := 1
	if len(args) > 0 {
		if n, err := strconv.Atoi(args[0]); err == nil && n > 0 {
			page = n
		}
	}
	card, err := e.renderListCard(msg.SessionKey, page)
	if err != nil {
		e.reply(p, msg.ReplyCtx, err.Error())
		return
	}
	e.replyWithCard(p, msg.ReplyCtx, card)
}

func (e *Engine) cmdSwitch(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, "Usage: /switch <number | id_prefix | name>")
		return
	}
	query := strings.TrimSpace(strings.Join(args, " "))

	slog.Info("cmdSwitch: listing agent sessions", "session_key", msg.SessionKey)
	agent, interactiveKey, err := e.commandContext(p, msg)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsResolutionError, err))
		return
	}
	agentSessions, err := agent.ListSessions(e.ctx)
	if err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v", err))
		return
	}

	matched := e.matchSession(agentSessions, query)

	if matched == nil && e.isAdmin(msg.UserID) {
		allConvs := e.conversations.List()
		if idx, err := strconv.Atoi(query); err == nil && idx >= 1 && idx <= len(allConvs) {
			targetConv := allConvs[idx-1]
			if targetConv.Key != msg.SessionKey {
				e.SetAdminOverride(msg.SessionKey, targetConv.Key)
				e.reply(p, msg.ReplyCtx, fmt.Sprintf(
					"🔀 已进入 [%s] 的会话。\n你的消息将路由到该用户的 Agent。\n使用 /approve 批准 · /reject 拒绝 · /leave 退出", targetConv.OwnerID))
				return
			}
		}
	}

	if matched == nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgSwitchNoMatch), query))
		return
	}

	slog.Info("cmdSwitch: cleaning up old session", "session_key", msg.SessionKey)
	e.cleanupConversation(interactiveKey)
	slog.Info("cmdSwitch: cleanup done", "session_key", msg.SessionKey)

	conv := e.conversations.GetOrCreate(interactiveKey)
	conv.SetAgentInfo(matched.ID, matched.Summary)
	conv.ClearHistory()
	e.conversations.Save()

	shortID := matched.ID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	displayName := e.conversations.GetSessionName(matched.ID)
	if displayName == "" {
		displayName = matched.Summary
	}
	e.reply(p, msg.ReplyCtx,
		e.i18n.Tf(MsgSwitchSuccess, displayName, shortID, matched.MessageCount))
}

// matchSession resolves a user query to an agent session. Priority:
//  1. Numeric index (1-based, matching /list output)
//  2. Exact custom name match (case-insensitive)
//  3. Session ID prefix match
//  4. Custom name prefix match (case-insensitive)
//  5. Summary substring match (case-insensitive)
func (e *Engine) matchSession(sessions []AgentSessionInfo, query string) *AgentSessionInfo {
	if len(sessions) == 0 {
		return nil
	}

	// 1. Numeric index
	if idx, err := strconv.Atoi(query); err == nil && idx >= 1 && idx <= len(sessions) {
		return &sessions[idx-1]
	}

	queryLower := strings.ToLower(query)

	// 2. Exact custom name match
	for i := range sessions {
		name := e.conversations.GetSessionName(sessions[i].ID)
		if name != "" && strings.ToLower(name) == queryLower {
			return &sessions[i]
		}
	}

	// 3. Session ID prefix match
	for i := range sessions {
		if strings.HasPrefix(sessions[i].ID, query) {
			return &sessions[i]
		}
	}

	// 4. Custom name prefix match
	for i := range sessions {
		name := e.conversations.GetSessionName(sessions[i].ID)
		if name != "" && strings.HasPrefix(strings.ToLower(name), queryLower) {
			return &sessions[i]
		}
	}

	// 5. Summary substring match
	for i := range sessions {
		if sessions[i].Summary != "" && strings.Contains(strings.ToLower(sessions[i].Summary), queryLower) {
			return &sessions[i]
		}
	}

	return nil
}

func (e *Engine) cmdShell(p Platform, msg *Message, raw string) {
	// Strip the command prefix ("/shell ", "/sh ", "/exec ", "/run ")
	shellCmd := raw
	for _, prefix := range []string{"/shell ", "/sh ", "/exec ", "/run "} {
		if strings.HasPrefix(strings.ToLower(raw), prefix) {
			shellCmd = raw[len(prefix):]
			break
		}
	}
	shellCmd = strings.TrimSpace(shellCmd)

	if shellCmd == "" {
		e.reply(p, msg.ReplyCtx, "Usage: /shell <command>\nExample: /shell ls -la")
		return
	}

	// In multi-workspace mode, resolve workspace directory for this channel
	var workDir string
	if e.multiWorkspace {
		channelID := extractChannelID(msg.SessionKey)
		projectKey := "project:" + e.name
		if b := e.workspaceBindings.Lookup(projectKey, channelID); b != nil {
			workDir = b.Workspace
		}
	}
	if workDir == "" {
		if wd, ok := e.agent.(interface{ GetWorkDir() string }); ok {
			workDir = wd.GetWorkDir()
		}
	}
	if workDir == "" {
		workDir, _ = os.Getwd()
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, "sh", "-c", shellCmd)
		cmd.Dir = workDir
		output, err := cmd.CombinedOutput()

		if ctx.Err() == context.DeadlineExceeded {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCommandTimeout), shellCmd))
			return
		}

		result := strings.TrimSpace(string(output))
		if err != nil && result == "" {
			result = err.Error()
		}
		if result == "" {
			result = "(no output)"
		}
		if runes := []rune(result); len(runes) > 4000 {
			result = string(runes[:3997]) + "..."
		}

		e.reply(p, msg.ReplyCtx, fmt.Sprintf("$ %s\n```\n%s\n```", shellCmd, result))
	}()
}

// cmdSearch searches sessions by name or message content.
// Usage: /search <keyword>
func (e *Engine) cmdSearch(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgSearchUsage))
		return
	}

	keyword := strings.ToLower(strings.Join(args, " "))

	// Get all agent sessions
	agent, _, err := e.commandContext(p, msg)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsResolutionError, err))
		return
	}
	agentSessions, err := agent.ListSessions(e.ctx)
	if err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgSearchError), err))
		return
	}

	type searchResult struct {
		id           string
		name         string
		summary      string
		matchType    string // "name" or "message"
		messageCount int
	}

	var results []searchResult

	for _, s := range agentSessions {
		// Check session name (custom name or summary)
		customName := e.conversations.GetSessionName(s.ID)
		displayName := customName
		if displayName == "" {
			displayName = s.Summary
		}

		// Match by name/summary
		if strings.Contains(strings.ToLower(displayName), keyword) {
			results = append(results, searchResult{
				id:           s.ID,
				name:         displayName,
				summary:      s.Summary,
				matchType:    "name",
				messageCount: s.MessageCount,
			})
			continue
		}

		// Match by session ID prefix
		if strings.HasPrefix(strings.ToLower(s.ID), keyword) {
			results = append(results, searchResult{
				id:           s.ID,
				name:         displayName,
				summary:      s.Summary,
				matchType:    "id",
				messageCount: s.MessageCount,
			})
			continue
		}
	}

	if len(results) == 0 {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgSearchNoResult), keyword))
		return
	}

	// Build result message
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(e.i18n.T(MsgSearchResult), len(results), keyword))

	for i, r := range results {
		shortID := r.id
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		sb.WriteString(fmt.Sprintf("\n%d. [%s] %s", i+1, shortID, r.name))
	}

	sb.WriteString("\n\n" + e.i18n.T(MsgSearchHint))

	e.reply(p, msg.ReplyCtx, sb.String())
}

func (e *Engine) cmdName(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNameUsage))
		return
	}

	agent, _, err := e.commandContext(p, msg)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsResolutionError, err))
		return
	}

	// Check if first arg is a number → naming a specific session by list index
	var targetID string
	var name string

	if idx, err := strconv.Atoi(args[0]); err == nil && idx >= 1 {
		// /name <number> <name...>
		if len(args) < 2 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNameUsage))
			return
		}
		agentSessions, err := agent.ListSessions(e.ctx)
		if err != nil {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v", err))
			return
		}
		if idx > len(agentSessions) {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgSwitchNoSession), idx))
			return
		}
		targetID = agentSessions[idx-1].ID
		name = strings.Join(args[1:], " ")
	} else {
		// /name <name...> → current session
		conv := e.conversations.GetOrCreate(msg.SessionKey)
		targetID = conv.AgentSessionID
		if targetID == "" {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNameNoSession))
			return
		}
		name = strings.Join(args, " ")
	}

	name = strings.TrimSpace(name)
	if name == "" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNameUsage))
		return
	}

	e.conversations.SetSessionName(targetID, name)
	e.conversations.Save()

	shortID := targetID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgNameSet), name, shortID))
}

func (e *Engine) cmdCurrent(p Platform, msg *Message) {
	if !supportsCards(p) {
		conv := e.conversations.GetOrCreate(msg.SessionKey)
		conv.mu.Lock()
		agentID := conv.AgentSessionID
		name := conv.Name
		histLen := len(conv.History)
		conv.mu.Unlock()
		if agentID == "" {
			agentID = e.i18n.T(MsgSessionNotStarted)
		}
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCurrentSession), name, agentID, histLen))
		return
	}

	e.replyWithCard(p, msg.ReplyCtx, e.renderCurrentCard(msg.SessionKey))
}

func (e *Engine) cmdStatus(p Platform, msg *Message) {
	if !supportsCards(p) {
		agent, _, err := e.commandContext(p, msg)
		if err != nil {
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsResolutionError, err))
			return
		}
		platNames := make([]string, len(e.platforms))
		for i, pl := range e.platforms {
			platNames[i] = pl.Name()
		}
		platformStr := strings.Join(platNames, ", ")
		if len(platNames) == 0 {
			platformStr = "-"
		}

		uptimeStr := formatDurationI18n(time.Since(e.startedAt), e.i18n.CurrentLang())

		cur := e.i18n.CurrentLang()
		langStr := fmt.Sprintf("%s (%s)", string(cur), langDisplayName(cur))

		var modeStr string
		if ms, ok := agent.(ModeSwitcher); ok {
			mode := ms.GetMode()
			if mode != "" {
				modeStr = e.i18n.Tf(MsgStatusMode, mode)
			}
		}

		e.quietMu.RLock()
		globalQuiet := e.quiet
		e.quietMu.RUnlock()

		conv := e.conversations.Get(msg.SessionKey)

		sessionQuiet := false
		if conv != nil {
			conv.mu.Lock()
			sessionQuiet = conv.Quiet
			conv.mu.Unlock()
		}

		quietStr := e.i18n.T(MsgQuietOffShort)
		if globalQuiet || sessionQuiet {
			quietStr = e.i18n.T(MsgQuietOnShort)
		}
		modeStr += e.i18n.Tf(MsgStatusQuiet, quietStr)

		sessionDisplayName := ""
		historyLen := 0
		if conv != nil {
			conv.mu.Lock()
			sessionDisplayName = conv.Name
			historyLen = len(conv.History)
			conv.mu.Unlock()
		}
		sessionStr := e.i18n.Tf(MsgStatusSession, sessionDisplayName, historyLen)

		var cronStr string
		if e.cronScheduler != nil {
			jobs := e.cronScheduler.Store().ListBySessionKey(msg.SessionKey)
			if len(jobs) > 0 {
				enabledCount := 0
				for _, j := range jobs {
					if j.Enabled {
						enabledCount++
					}
				}
				cronStr = e.i18n.Tf(MsgStatusCron, len(jobs), enabledCount)
			}
		}

		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgStatusTitle,
			e.name,
			agent.Name(),
			platformStr,
			uptimeStr,
			langStr,
			modeStr,
			sessionStr,
			cronStr,
		))
		return
	}

	e.replyWithCard(p, msg.ReplyCtx, e.renderStatusCard(msg.SessionKey))
}

func (e *Engine) cmdUsage(p Platform, msg *Message) {
	reporter, ok := e.agent.(UsageReporter)
	if !ok {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgUsageNotSupported))
		return
	}

	fetchCtx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()

	report, err := reporter.GetUsage(fetchCtx)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgUsageFetchFailed, err))
		return
	}

	if supportsCards(p) {
		e.replyWithCard(p, msg.ReplyCtx, e.renderUsageCard(report))
		return
	}

	e.reply(p, msg.ReplyCtx, formatUsageReport(report, e.i18n.CurrentLang()))
}

func formatUsageReport(report *UsageReport, lang Language) string {
	if report == nil {
		return usageUnavailableText(lang)
	}

	var sb strings.Builder
	sb.WriteString(usageAccountLabel(lang))
	sb.WriteString(accountDisplay(report))
	sb.WriteString(formatUsageBlocks(report, lang))

	return strings.TrimSpace(sb.String())
}

func formatUsageBlocks(report *UsageReport, lang Language) string {
	primary, secondary := selectUsageWindows(report)
	var sections []string
	if primary != nil {
		sections = append(sections, formatUsageBlock(lang, primary))
	}
	if secondary != nil {
		sections = append(sections, formatUsageBlock(lang, secondary))
	}
	if len(sections) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(sections, "\n\n")
}

func accountDisplay(report *UsageReport) string {
	var base string
	if report.Email != "" {
		base = report.Email
	} else if report.AccountID != "" {
		base = report.AccountID
	} else if report.UserID != "" {
		base = report.UserID
	} else {
		base = "-"
	}
	if report.Plan != "" {
		return fmt.Sprintf("%s (%s)", base, report.Plan)
	}
	return base
}

func selectUsageWindows(report *UsageReport) (*UsageWindow, *UsageWindow) {
	for _, bucket := range report.Buckets {
		if len(bucket.Windows) == 0 {
			continue
		}
		var primary, secondary *UsageWindow
		for i := range bucket.Windows {
			window := &bucket.Windows[i]
			switch window.WindowSeconds {
			case 18000:
				primary = window
			case 604800:
				if secondary == nil {
					secondary = window
				}
			}
		}
		if primary == nil && len(bucket.Windows) > 0 {
			primary = &bucket.Windows[0]
		}
		if secondary == nil && len(bucket.Windows) > 1 {
			secondary = &bucket.Windows[1]
		}
		if primary != nil || secondary != nil {
			return primary, secondary
		}
	}
	return nil, nil
}

func formatUsageBlock(lang Language, window *UsageWindow) string {
	remaining := 100 - window.UsedPercent
	if remaining < 0 {
		remaining = 0
	}
	var sb strings.Builder
	sb.WriteString(usageWindowLabel(lang, window.WindowSeconds))
	sb.WriteString("\n")
	sb.WriteString(usageRemainingLabel(lang))
	sb.WriteString(usageColon(lang))
	sb.WriteString(fmt.Sprintf("%d%%", remaining))
	sb.WriteString("\n")
	sb.WriteString(usageResetLabel(lang))
	sb.WriteString(usageColon(lang))
	sb.WriteString(formatUsageResetTime(lang, window.ResetAfterSeconds))
	return sb.String()
}

func (e *Engine) renderUsageCard(report *UsageReport) *Card {
	lang := e.i18n.CurrentLang()
	return NewCard().
		Title(usageCardTitle(lang), "indigo").
		Markdown(strings.TrimSpace(formatUsageReport(report, lang))).
		Buttons(e.cardBackButton()).
		Build()
}

func formatUsageResetTime(lang Language, resetAfterSeconds int) string {
	if resetAfterSeconds <= 0 {
		switch lang {
		case LangChinese, LangTraditionalChinese:
			return "-"
		case LangJapanese:
			return "-"
		case LangSpanish:
			return "-"
		default:
			return "-"
		}
	}
	return formatDurationI18n(time.Duration(resetAfterSeconds)*time.Second, lang)
}

func usageAccountLabel(lang Language) string {
	switch lang {
	case LangChinese:
		return "账号："
	case LangTraditionalChinese:
		return "帳號："
	case LangJapanese:
		return "アカウント: "
	case LangSpanish:
		return "Cuenta: "
	default:
		return "Account: "
	}
}

func usageWindowLabel(lang Language, seconds int) string {
	switch seconds {
	case 18000:
		switch lang {
		case LangChinese:
			return "5小时限额"
		case LangTraditionalChinese:
			return "5小時限額"
		case LangJapanese:
			return "5時間枠"
		case LangSpanish:
			return "Límite 5h"
		default:
			return "5h limit"
		}
	case 604800:
		switch lang {
		case LangChinese:
			return "7日限额"
		case LangTraditionalChinese:
			return "7日限額"
		case LangJapanese:
			return "7日枠"
		case LangSpanish:
			return "Límite 7d"
		default:
			return "7d limit"
		}
	default:
		switch lang {
		case LangChinese, LangTraditionalChinese:
			return formatDurationI18n(time.Duration(seconds)*time.Second, lang) + "限额"
		case LangJapanese:
			return formatDurationI18n(time.Duration(seconds)*time.Second, lang) + "枠"
		case LangSpanish:
			return "Límite " + formatDurationI18n(time.Duration(seconds)*time.Second, lang)
		default:
			return formatDurationI18n(time.Duration(seconds)*time.Second, lang) + " limit"
		}
	}
}

func usageRemainingLabel(lang Language) string {
	switch lang {
	case LangChinese:
		return "剩余"
	case LangTraditionalChinese:
		return "剩餘"
	case LangJapanese:
		return "残り"
	case LangSpanish:
		return "restante"
	default:
		return "Remaining"
	}
}

func usageResetLabel(lang Language) string {
	switch lang {
	case LangChinese:
		return "重置"
	case LangTraditionalChinese:
		return "重置"
	case LangJapanese:
		return "リセット"
	case LangSpanish:
		return "Reinicio"
	default:
		return "Resets"
	}
}

func usageColon(lang Language) string {
	switch lang {
	case LangChinese, LangTraditionalChinese:
		return "："
	default:
		return ": "
	}
}

func usageCardTitle(lang Language) string {
	switch lang {
	case LangChinese:
		return "Usage"
	case LangTraditionalChinese:
		return "Usage"
	case LangJapanese:
		return "Usage"
	case LangSpanish:
		return "Usage"
	default:
		return "Usage"
	}
}

func usageUnavailableText(lang Language) string {
	switch lang {
	case LangChinese:
		return "暂无 usage 信息。"
	case LangTraditionalChinese:
		return "暫無 usage 資訊。"
	case LangJapanese:
		return "usage 情報はありません。"
	case LangSpanish:
		return "No hay datos de usage."
	default:
		return "Usage unavailable."
	}
}

func splitCardTitleBody(content string) (string, string) {
	content = strings.TrimSpace(content)
	parts := strings.SplitN(content, "\n\n", 2)
	title := strings.TrimSpace(parts[0])
	if len(parts) == 1 {
		return title, ""
	}
	return title, strings.TrimSpace(parts[1])
}

func (e *Engine) cardBackButton() CardButton {
	return DefaultBtn(e.i18n.T(MsgCardBack), "nav:/help")
}

func (e *Engine) cardPrevButton(action string) CardButton {
	return DefaultBtn(e.i18n.T(MsgCardPrev), action)
}

func (e *Engine) cardNextButton(action string) CardButton {
	return DefaultBtn(e.i18n.T(MsgCardNext), action)
}

// simpleCard builds a card with a title, markdown body and a single Back button.
// Used to reduce repetition across render functions that share this pattern.
func (e *Engine) simpleCard(title, color, content string) *Card {
	return NewCard().Title(title, color).Markdown(content).Buttons(e.cardBackButton()).Build()
}

// renderListCardSafe wraps renderListCard and returns an error card on failure.
func (e *Engine) renderListCardSafe(sessionKey string, page int) *Card {
	card, err := e.renderListCard(sessionKey, page)
	if err != nil {
		agent := e.sessionContextForKey(sessionKey)
		return e.simpleCard(e.i18n.Tf(MsgCardTitleSessions, agent.Name(), 0), "red", err.Error())
	}
	return card
}

func (e *Engine) renderStatusCard(sessionKey string) *Card {
	agent := e.sessionContextForKey(sessionKey)
	platNames := make([]string, len(e.platforms))
	for i, pl := range e.platforms {
		platNames[i] = pl.Name()
	}
	platformStr := strings.Join(platNames, ", ")
	if len(platNames) == 0 {
		platformStr = "-"
	}

	uptimeStr := formatDurationI18n(time.Since(e.startedAt), e.i18n.CurrentLang())

	cur := e.i18n.CurrentLang()
	langStr := fmt.Sprintf("%s (%s)", string(cur), langDisplayName(cur))

	var modeStr string
	if ms, ok := agent.(ModeSwitcher); ok {
		mode := ms.GetMode()
		if mode != "" {
			modeStr = e.i18n.Tf(MsgStatusMode, mode)
		}
	}

	e.quietMu.RLock()
	globalQuiet := e.quiet
	e.quietMu.RUnlock()

	conv := e.conversations.Get(sessionKey)

	sessionQuiet := false
	if conv != nil {
		conv.mu.Lock()
		sessionQuiet = conv.Quiet
		conv.mu.Unlock()
	}

	quietStr := e.i18n.T(MsgQuietOffShort)
	if globalQuiet || sessionQuiet {
		quietStr = e.i18n.T(MsgQuietOnShort)
	}
	modeStr += e.i18n.Tf(MsgStatusQuiet, quietStr)

	sessionDisplayName := ""
	historyLen := 0
	if conv != nil {
		conv.mu.Lock()
		sessionDisplayName = conv.Name
		historyLen = len(conv.History)
		conv.mu.Unlock()
	}
	sessionStr := e.i18n.Tf(MsgStatusSession, sessionDisplayName, historyLen)

	var cronStr string
	if e.cronScheduler != nil {
		jobs := e.cronScheduler.Store().ListBySessionKey(sessionKey)
		if len(jobs) > 0 {
			enabledCount := 0
			for _, j := range jobs {
				if j.Enabled {
					enabledCount++
				}
			}
			cronStr = e.i18n.Tf(MsgStatusCron, len(jobs), enabledCount)
		}
	}

	statusText := e.i18n.Tf(MsgStatusTitle,
		e.name,
		agent.Name(),
		platformStr,
		uptimeStr,
		langStr,
		modeStr,
		sessionStr,
		cronStr,
	)
	title, body := splitCardTitleBody(statusText)

	return NewCard().
		Title(title, "green").
		Markdown(body).
		Buttons(e.cardBackButton()).
		Build()
}

func cronTimeFormat(t, now time.Time) string {
	if t.Year() != now.Year() {
		return "2006-01-02 15:04"
	}
	return "01-02 15:04"
}

func formatDurationI18n(d time.Duration, lang Language) string {
	d = d.Round(time.Second)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60

	switch lang {
	case LangChinese, LangTraditionalChinese:
		if days > 0 {
			return fmt.Sprintf("%d天 %d小时 %d分钟", days, hours, minutes)
		}
		if hours > 0 {
			return fmt.Sprintf("%d小时 %d分钟", hours, minutes)
		}
		return fmt.Sprintf("%d分钟", minutes)
	case LangJapanese:
		if days > 0 {
			return fmt.Sprintf("%d日 %d時間 %d分", days, hours, minutes)
		}
		if hours > 0 {
			return fmt.Sprintf("%d時間 %d分", hours, minutes)
		}
		return fmt.Sprintf("%d分", minutes)
	case LangSpanish:
		if days > 0 {
			return fmt.Sprintf("%d días %dh %dm", days, hours, minutes)
		}
		if hours > 0 {
			return fmt.Sprintf("%dh %dm", hours, minutes)
		}
		return fmt.Sprintf("%dm", minutes)
	default:
		if days > 0 {
			return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
		}
		if hours > 0 {
			return fmt.Sprintf("%dh %dm", hours, minutes)
		}
		return fmt.Sprintf("%dm", minutes)
	}
}

func (e *Engine) cmdHistory(p Platform, msg *Message, args []string) {
	if len(args) == 0 && supportsCards(p) {
		e.replyWithCard(p, msg.ReplyCtx, e.renderHistoryCard(msg.SessionKey))
		return
	}
	if len(args) == 0 {
		args = []string{"10"}
	}

	agent, _, err := e.commandContext(p, msg)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsResolutionError, err))
		return
	}
	conv := e.conversations.GetOrCreate(msg.SessionKey)
	n := 10
	if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
		n = v
	}

	entries := conv.GetHistory(n)
	if len(entries) == 0 && conv.AgentSessionID != "" {
		if hp, ok := agent.(HistoryProvider); ok {
			if agentEntries, err := hp.GetSessionHistory(e.ctx, conv.AgentSessionID, n); err == nil {
				entries = agentEntries
			}
		}
	}

	if len(entries) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgHistoryEmpty))
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📜 History (last %d):\n\n", len(entries)))
	for _, h := range entries {
		icon := "👤"
		if h.Role == "assistant" {
			icon = "🤖"
		}
		content := h.Content
		if len([]rune(content)) > 200 {
			content = string([]rune(content)[:200]) + "..."
		}
		sb.WriteString(fmt.Sprintf("%s [%s]\n%s\n\n", icon, h.Timestamp.Format("15:04:05"), content))
	}
	e.reply(p, msg.ReplyCtx, sb.String())
}

func (e *Engine) cmdLang(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		cur := e.i18n.CurrentLang()
		name := langDisplayName(cur)
		text := e.i18n.Tf(MsgLangCurrent, name)
		buttons := [][]ButtonOption{
			{
				{Text: "English", Data: "cmd:/lang en"},
				{Text: "中文", Data: "cmd:/lang zh"},
				{Text: "繁體中文", Data: "cmd:/lang zh-TW"},
			},
			{
				{Text: "日本語", Data: "cmd:/lang ja"},
				{Text: "Español", Data: "cmd:/lang es"},
				{Text: "Auto", Data: "cmd:/lang auto"},
			},
		}
		if supportsCards(p) {
			e.replyWithCard(p, msg.ReplyCtx, e.renderLangCard())
			return
		}
		if _, ok := p.(InlineButtonSender); ok {
			e.replyWithButtons(p, msg.ReplyCtx, text, buttons)
			return
		}
		var sb strings.Builder
		sb.WriteString(text)
		sb.WriteString("\n\n")
		sb.WriteString("- English: `/lang en`\n")
		sb.WriteString("- 中文: `/lang zh`\n")
		sb.WriteString("- 繁體中文: `/lang zh-TW`\n")
		sb.WriteString("- 日本語: `/lang ja`\n")
		sb.WriteString("- Español: `/lang es`\n")
		sb.WriteString("- Auto: `/lang auto`")
		e.reply(p, msg.ReplyCtx, sb.String())
		return
	}

	target := strings.ToLower(strings.TrimSpace(args[0]))
	var lang Language
	switch target {
	case "en", "english":
		lang = LangEnglish
	case "zh", "cn", "chinese", "中文":
		lang = LangChinese
	case "zh-tw", "zh_tw", "zhtw", "繁體", "繁体":
		lang = LangTraditionalChinese
	case "ja", "jp", "japanese", "日本語":
		lang = LangJapanese
	case "es", "spanish", "español":
		lang = LangSpanish
	case "auto":
		lang = LangAuto
	default:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgLangInvalid))
		return
	}

	e.i18n.SetLang(lang)
	name := langDisplayName(lang)
	e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgLangChanged, name))
}

func langDisplayName(lang Language) string {
	switch lang {
	case LangEnglish:
		return "English"
	case LangChinese:
		return "中文"
	case LangTraditionalChinese:
		return "繁體中文"
	case LangJapanese:
		return "日本語"
	case LangSpanish:
		return "Español"
	default:
		return "Auto"
	}
}

func (e *Engine) cmdHelp(p Platform, msg *Message) {
	if !supportsCards(p) {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgHelp))
		return
	}
	e.replyWithCard(p, msg.ReplyCtx, e.renderHelpCard())
}

const defaultHelpGroup = "session"

type helpCardItem struct {
	command string
	action  string
}

type helpCardGroup struct {
	key      string
	titleKey MsgKey
	items    []helpCardItem
}

func helpCardGroups() []helpCardGroup {
	return []helpCardGroup{
		{
			key:      "session",
			titleKey: MsgHelpSessionSection,
			items: []helpCardItem{
				{command: "/new", action: "act:/new"},
				{command: "/list", action: "nav:/list"},
				{command: "/current", action: "nav:/current"},
				{command: "/switch", action: "nav:/list"},
				{command: "/search", action: "cmd:/search"},
				{command: "/history", action: "nav:/history"},
				{command: "/delete", action: "cmd:/delete"},
				{command: "/name", action: "cmd:/name"},
			},
		},
		{
			key:      "agent",
			titleKey: MsgHelpAgentSection,
			items: []helpCardItem{
				{command: "/model", action: "nav:/model"},
				{command: "/agent", action: "nav:/agent"},
				{command: "/reasoning", action: "nav:/reasoning"},
				{command: "/mode", action: "nav:/mode"},
				{command: "/lang", action: "nav:/lang"},
				{command: "/provider", action: "nav:/provider"},
				{command: "/memory", action: "cmd:/memory"},
				{command: "/quiet", action: "act:/quiet"},
			},
		},
		{
			key:      "tools",
			titleKey: MsgHelpToolsSection,
			items: []helpCardItem{
				{command: "/cron", action: "nav:/cron"},
				{command: "/commands", action: "nav:/commands"},
				{command: "/alias", action: "nav:/alias"},
				{command: "/skills", action: "nav:/skills"},
				{command: "/compress", action: "cmd:/compress"},
				{command: "/stop", action: "act:/stop"},
			},
		},
		{
			key:      "system",
			titleKey: MsgHelpSystemSection,
			items: []helpCardItem{
				{command: "/status", action: "nav:/status"},
				{command: "/doctor", action: "nav:/doctor"},
				{command: "/usage", action: "cmd:/usage"},
				{command: "/config", action: "nav:/config"},
				{command: "/version", action: "nav:/version"},
				{command: "/upgrade", action: "nav:/upgrade"},
				{command: "/restart", action: "cmd:/restart"},
			},
		},
	}
}

func (e *Engine) renderHelpCard() *Card {
	return e.renderHelpGroupCard(defaultHelpGroup)
}

// splitHelpTabRows splits tab buttons into rows. Card-based platforms
// get 2 buttons per row for better layout; others get all in one row.
func splitHelpTabRows(useMultiRow bool, tabs []CardButton) [][]CardButton {
	if useMultiRow {
		rows := make([][]CardButton, 0, (len(tabs)+1)/2)
		for i := 0; i < len(tabs); i += 2 {
			end := i + 2
			if end > len(tabs) {
				end = len(tabs)
			}
			rows = append(rows, tabs[i:end])
		}
		return rows
	}
	return [][]CardButton{tabs}
}

func (e *Engine) renderHelpGroupCard(groupKey string) *Card {
	sectionTitle := func(key MsgKey) string {
		section := e.i18n.T(key)
		if idx := strings.IndexByte(section, '\n'); idx >= 0 {
			return section[:idx]
		}
		return section
	}
	tabLabel := func(key MsgKey) string {
		return strings.Trim(sectionTitle(key), "* ")
	}
	commandText := func(command string) string {
		return "**" + command + "**  " + e.i18n.T(MsgKey(strings.TrimPrefix(command, "/")))
	}

	groups := helpCardGroups()
	current := groups[0]
	normalizedGroup := strings.ToLower(strings.TrimSpace(groupKey))
	for _, group := range groups {
		if group.key == normalizedGroup {
			current = group
			break
		}
	}

	cb := NewCard().Title(e.i18n.T(MsgHelpTitle), "blue")
	var tabs []CardButton
	for _, group := range groups {
		btnType := "default"
		if group.key == current.key {
			btnType = "primary"
		}
		tabs = append(tabs, Btn(tabLabel(group.titleKey), btnType, "nav:/help "+group.key))
	}
	for _, row := range splitHelpTabRows(true, tabs) {
		cb.ButtonsEqual(row...)
	}
	for _, item := range current.items {
		cb.ListItem(commandText(item.command), "▶", item.action)
	}

	cb.Divider()
	cb.Buttons(
		PrimaryBtn(e.i18n.T(MsgQuickNew), "act:/new"),
		DefaultBtn(e.i18n.T(MsgQuickSwitch), "nav:/list"),
		DefaultBtn(e.i18n.T(MsgQuickModel), "nav:/model"),
		DefaultBtn(e.i18n.T(MsgQuickAgent), "nav:/agent"),
		DefaultBtn(e.i18n.T(MsgQuickMore), "nav:/help tools"),
	)

	cb.Note(e.i18n.T(MsgHelpTip))
	return cb.Build()
}

// GetAllCommands returns all available commands for bot menu registration.
// It includes built-in commands (with localized descriptions) and custom commands.
func (e *Engine) GetAllCommands() []BotCommandInfo {
	var commands []BotCommandInfo

	// Collect built-in  commands (use primary name, first in names list)
	seenCmds := make(map[string]bool)
	for _, c := range builtinCommands {
		if len(c.names) == 0 {
			continue
		}
		// Use id as primary
		primaryName := c.id
		if seenCmds[primaryName] {
			continue
		}
		seenCmds[primaryName] = true

		// Skip disabled commands
		if e.disabledCmds[c.id] {
			continue
		}

		commands = append(commands, BotCommandInfo{
			Command:     primaryName,
			Description: e.i18n.T(MsgKey(primaryName)),
		})
	}

	// Collect custom commands from CommandRegistry
	for _, c := range e.commands.ListAll() {
		if seenCmds[strings.ToLower(c.Name)] {
			continue
		}
		seenCmds[strings.ToLower(c.Name)] = true

		desc := c.Description
		if desc == "" {
			desc = "Custom command"
		}

		commands = append(commands, BotCommandInfo{
			Command:     c.Name,
			Description: desc,
		})
	}

	// Collect skills
	for _, s := range e.skills.ListAll() {
		if seenCmds[strings.ToLower(s.Name)] {
			continue
		}
		seenCmds[strings.ToLower(s.Name)] = true

		desc := s.Description
		if desc == "" {
			desc = "Skill"
		}

		commands = append(commands, BotCommandInfo{
			Command:     s.Name,
			Description: desc,
		})
	}

	return commands
}

func (e *Engine) cmdModel(p Platform, msg *Message, args []string) {
	switcher, ok := e.agent.(ModelSwitcher)
	if !ok {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgModelNotSupported))
		return
	}

	if len(args) == 0 {
		if !supportsCards(p) {
			fetchCtx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
			defer cancel()
			models := switcher.AvailableModels(fetchCtx)

			var sb strings.Builder
			current := switcher.GetModel()
			if current == "" {
				sb.WriteString(e.i18n.T(MsgModelDefault))
			} else {
				sb.WriteString(e.i18n.Tf(MsgModelCurrent, current))
				sb.WriteString("\n")
			}
			sb.WriteString("\n")
			sb.WriteString(e.i18n.T(MsgModelListTitle))
			var buttons [][]ButtonOption
			var row []ButtonOption
			for i, m := range models {
				marker := "  "
				if m.Name == current {
					marker = "> "
				}
				desc := m.Desc
				if desc != "" {
					desc = " — " + desc
				}
				sb.WriteString(fmt.Sprintf("%s%d. %s%s\n", marker, i+1, m.Name, desc))

				label := m.Name
				if m.Name == current {
					label = "▶ " + label
				}
				row = append(row, ButtonOption{Text: label, Data: fmt.Sprintf("cmd:/model %d", i+1)})
				if len(row) >= 3 {
					buttons = append(buttons, row)
					row = nil
				}
			}
			if len(row) > 0 {
				buttons = append(buttons, row)
			}
			sb.WriteString("\n")
			sb.WriteString(e.i18n.T(MsgModelUsage))
			e.replyWithButtons(p, msg.ReplyCtx, sb.String(), buttons)
			return
		}
		e.replyWithCard(p, msg.ReplyCtx, e.renderModelCard())
		return
	}

	fetchCtx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	models := switcher.AvailableModels(fetchCtx)

	target := args[0]
	if idx, err := strconv.Atoi(target); err == nil && idx >= 1 && idx <= len(models) {
		target = models[idx-1].Name
	}

	switcher.SetModel(target)
	e.cleanupConversation(msg.SessionKey)

	conv := e.conversations.GetOrCreate(msg.SessionKey)
	conv.SetAgentSessionID("")
	conv.ClearHistory()
	e.conversations.Save()

	e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgModelChanged, target))
}

func (e *Engine) cmdReasoning(p Platform, msg *Message, args []string) {
	switcher, ok := e.agent.(ReasoningEffortSwitcher)
	if !ok {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgReasoningNotSupported))
		return
	}

	if len(args) == 0 {
		if !supportsCards(p) {
			efforts := switcher.AvailableReasoningEfforts()

			var sb strings.Builder
			current := switcher.GetReasoningEffort()
			if current == "" {
				sb.WriteString(e.i18n.T(MsgReasoningDefault))
			} else {
				sb.WriteString(e.i18n.Tf(MsgReasoningCurrent, current))
				sb.WriteString("\n")
			}
			sb.WriteString("\n")
			sb.WriteString(e.i18n.T(MsgReasoningListTitle))
			var buttons [][]ButtonOption
			var row []ButtonOption
			for i, effort := range efforts {
				marker := "  "
				if effort == current {
					marker = "> "
				}
				sb.WriteString(fmt.Sprintf("%s%d. %s\n", marker, i+1, effort))

				label := effort
				if effort == current {
					label = "▶ " + label
				}
				row = append(row, ButtonOption{Text: label, Data: fmt.Sprintf("cmd:/reasoning %d", i+1)})
				if len(row) >= 3 {
					buttons = append(buttons, row)
					row = nil
				}
			}
			if len(row) > 0 {
				buttons = append(buttons, row)
			}
			sb.WriteString("\n")
			sb.WriteString(e.i18n.T(MsgReasoningUsage))
			e.replyWithButtons(p, msg.ReplyCtx, sb.String(), buttons)
			return
		}
		e.replyWithCard(p, msg.ReplyCtx, e.renderReasoningCard())
		return
	}

	efforts := switcher.AvailableReasoningEfforts()
	target := strings.ToLower(strings.TrimSpace(args[0]))
	if idx, err := strconv.Atoi(target); err == nil && idx >= 1 && idx <= len(efforts) {
		target = efforts[idx-1]
	}

	valid := false
	for _, effort := range efforts {
		if effort == target {
			valid = true
			break
		}
	}
	if !valid {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgReasoningUsage))
		return
	}

	switcher.SetReasoningEffort(target)
	e.cleanupConversation(msg.SessionKey)

	conv := e.conversations.GetOrCreate(msg.SessionKey)
	conv.SetAgentSessionID("")
	conv.ClearHistory()
	e.conversations.Save()

	e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgReasoningChanged, target))
}

func (e *Engine) cmdMode(p Platform, msg *Message, args []string) {
	switcher, ok := e.agent.(ModeSwitcher)
	if !ok {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgModeNotSupported))
		return
	}

	if len(args) == 0 {
		if !supportsCards(p) {
			current := switcher.GetMode()
			modes := switcher.PermissionModes()
			var sb strings.Builder
			zhLike := e.i18n.IsZhLike()
			for _, m := range modes {
				marker := "  "
				if m.Key == current {
					marker = "▶ "
				}
				if zhLike {
					sb.WriteString(fmt.Sprintf("%s**%s** — %s\n", marker, m.NameZh, m.DescZh))
				} else {
					sb.WriteString(fmt.Sprintf("%s**%s** — %s\n", marker, m.Name, m.Desc))
				}
			}
			sb.WriteString(e.i18n.T(MsgModeUsage))

			var buttons [][]ButtonOption
			var row []ButtonOption
			for _, m := range modes {
				label := m.Name
				if zhLike {
					label = m.NameZh
				}
				if m.Key == current {
					label = "▶ " + label
				}
				row = append(row, ButtonOption{Text: label, Data: "cmd:/mode " + m.Key})
				if len(row) >= 2 {
					buttons = append(buttons, row)
					row = nil
				}
			}
			if len(row) > 0 {
				buttons = append(buttons, row)
			}
			e.replyWithButtons(p, msg.ReplyCtx, sb.String(), buttons)
			return
		}
		e.replyWithCard(p, msg.ReplyCtx, e.renderModeCard())
		return
	}

	target := strings.ToLower(args[0])
	switcher.SetMode(target)
	newMode := switcher.GetMode()

	e.cleanupConversation(msg.SessionKey)

	modes := switcher.PermissionModes()
	displayName := newMode
	zhLike := e.i18n.IsZhLike()
	for _, m := range modes {
		if m.Key == newMode {
			if zhLike {
				displayName = m.NameZh
			} else {
				displayName = m.Name
			}
			break
		}
	}
	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgModeChanged), displayName))
}

func (e *Engine) cmdQuiet(p Platform, msg *Message, args []string) {
	// /quiet global — toggle global quiet for all sessions
	if len(args) > 0 && args[0] == "global" {
		e.quietMu.Lock()
		e.quiet = !e.quiet
		quiet := e.quiet
		e.quietMu.Unlock()

		if quiet {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgQuietGlobalOn))
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgQuietGlobalOff))
		}
		return
	}

	// /quiet — toggle per-session quiet
	conv := e.conversations.Get(msg.SessionKey)

	if conv == nil {
		conv = e.conversations.GetOrCreate(msg.SessionKey)
		conv.mu.Lock()
		conv.Quiet = true
		conv.mu.Unlock()
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgQuietOn))
		return
	}

	conv.mu.Lock()
	conv.Quiet = !conv.Quiet
	quiet := conv.Quiet
	conv.mu.Unlock()

	if quiet {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgQuietOn))
	} else {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgQuietOff))
	}
}

func (e *Engine) cmdTTS(p Platform, msg *Message, args []string) {
	if e.tts == nil || !e.tts.Enabled || e.tts.TTS == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTTSNotEnabled))
		return
	}
	if len(args) == 0 {
		providerStr := e.tts.Provider
		if providerStr == "" {
			providerStr = "unknown"
		}
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgTTSStatus), e.tts.GetTTSMode(), providerStr))
		return
	}
	switch args[0] {
	case "always", "voice_only":
		mode := args[0]
		e.tts.SetTTSMode(mode)
		if e.ttsSaveFunc != nil {
			if err := e.ttsSaveFunc(mode); err != nil {
				slog.Warn("tts: failed to persist mode", "error", err)
			}
		}
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgTTSSwitched), mode))
	default:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTTSUsage))
	}
}

func (e *Engine) cmdStop(p Platform, msg *Message) {
	conv := e.conversations.Get(msg.SessionKey)

	if conv == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNoExecution))
		return
	}

	conv.mu.Lock()
	pending := conv.PendingPerm
	quietMode := conv.Quiet
	if pending != nil {
		conv.PendingPerm = nil
	}
	conv.mu.Unlock()
	if pending != nil {
		pending.resolve()
	}

	e.cleanupConversation(msg.SessionKey)

	if quietMode {
		conv.mu.Lock()
		conv.Quiet = true
		conv.mu.Unlock()
	}

	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgExecutionStopped))
}

func (e *Engine) cmdCompress(p Platform, msg *Message) {
	compressor, ok := e.agent.(ContextCompressor)
	if !ok || compressor.CompressCommand() == "" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCompressNotSupported))
		return
	}

	conv := e.conversations.Get(msg.SessionKey)

	if conv == nil || conv.AgentSession == nil || !conv.AgentSession.Alive() {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCompressNoSession))
		return
	}

	if !conv.TryLock() {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPreviousProcessing))
		return
	}

	e.send(p, msg.ReplyCtx, e.i18n.T(MsgCompressing))

	go func() {
		defer conv.Unlock()

		conv.mu.Lock()
		conv.ReplyPlatform = p
		conv.ReplyCtx = msg.ReplyCtx
		conv.mu.Unlock()

		drainEvents(conv.AgentSession.Events())

		cmd := compressor.CompressCommand()
		if err := conv.AgentSession.Send(cmd, nil, nil); err != nil {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
			if !conv.AgentSession.Alive() {
				e.cleanupConversation(msg.SessionKey)
			}
			return
		}

		e.processCompressEvents(conv, msg.SessionKey, p, msg.ReplyCtx)
	}()
}

// processCompressEvents drains agent events after a compress command.
// Unlike processInteractiveEvents it does NOT record history and treats
// an empty result as success rather than "(empty response)".
func (e *Engine) processCompressEvents(conv *ConversationContext, sessionKey string, p Platform, replyCtx any) {
	var textParts []string
	events := conv.AgentSession.Events()

	var idleTimer *time.Timer
	var idleCh <-chan time.Time
	if e.eventIdleTimeout > 0 {
		idleTimer = time.NewTimer(e.eventIdleTimeout)
		defer idleTimer.Stop()
		idleCh = idleTimer.C
	}

	for {
		var event Event
		var ok bool

		select {
		case event, ok = <-events:
			if !ok {
				e.cleanupConversation(sessionKey, conv.AgentSession)
				if len(textParts) > 0 {
					e.send(p, replyCtx, strings.Join(textParts, ""))
				} else {
					e.reply(p, replyCtx, e.i18n.T(MsgCompressDone))
				}
				return
			}
		case <-idleCh:
			e.send(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgError), "compress timed out"))
			e.cleanupConversation(sessionKey, conv.AgentSession)
			return
		case <-e.ctx.Done():
			return
		}

		if idleTimer != nil {
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(e.eventIdleTimeout)
		}

		switch event.Type {
		case EventText:
			if event.Content != "" {
				textParts = append(textParts, event.Content)
			}
		case EventResult:
			result := event.Content
			if result == "" && len(textParts) > 0 {
				result = strings.Join(textParts, "")
			}
			if result != "" {
				e.send(p, replyCtx, result)
			} else {
				e.reply(p, replyCtx, e.i18n.T(MsgCompressDone))
			}
			return
		case EventError:
			if event.Error != nil {
				e.reply(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgError), event.Error))
			}
			return
		case EventPermissionRequest:
			_ = conv.AgentSession.RespondPermission(event.RequestID, PermissionResult{
				Behavior:     "allow",
				UpdatedInput: event.ToolInputRaw,
			})
		}
	}
}

func (e *Engine) cmdAllow(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		if auth, ok := e.agent.(ToolAuthorizer); ok {
			tools := auth.GetAllowedTools()
			if len(tools) == 0 {
				e.reply(p, msg.ReplyCtx, e.i18n.T(MsgNoToolsAllowed))
			} else {
				e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCurrentTools), strings.Join(tools, ", ")))
			}
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgToolAuthNotSupported))
		}
		return
	}

	toolName := strings.TrimSpace(args[0])
	if auth, ok := e.agent.(ToolAuthorizer); ok {
		if err := auth.AddAllowedTools(toolName); err != nil {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgToolAllowFailed), err))
			return
		}
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgToolAllowedNew), toolName))
	} else {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgToolAuthNotSupported))
	}
}

func (e *Engine) cmdProvider(p Platform, msg *Message, args []string) {
	switcher, ok := e.agent.(ProviderSwitcher)
	if !ok {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgProviderNotSupported))
		return
	}

	if len(args) == 0 {
		if supportsCards(p) {
			e.replyWithCard(p, msg.ReplyCtx, e.renderProviderCard())
			return
		}

		current := switcher.GetActiveProvider()
		providers := switcher.ListProviders()
		if current == nil && len(providers) == 0 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgProviderNone))
			return
		}

		var sb strings.Builder
		if current != nil {
			sb.WriteString(fmt.Sprintf(e.i18n.T(MsgProviderCurrent), current.Name))
			sb.WriteString("\n\n")
		}
		sb.WriteString(e.i18n.T(MsgProviderListTitle))
		for _, prov := range providers {
			marker := "  "
			if current != nil && prov.Name == current.Name {
				marker = "▶ "
			}
			detail := prov.Name
			if prov.BaseURL != "" {
				detail += " (" + prov.BaseURL + ")"
			}
			if prov.Model != "" {
				detail += " [" + prov.Model + "]"
			}
			sb.WriteString(fmt.Sprintf("%s%s\n", marker, detail))
		}
		sb.WriteString("\n" + e.i18n.T(MsgProviderSwitchHint))
		e.reply(p, msg.ReplyCtx, sb.String())
		return
	}

	sub := matchSubCommand(strings.ToLower(args[0]), []string{
		"list", "add", "remove", "switch", "current", "clear", "reset", "none",
	})
	switch sub {
	case "list":
		providers := switcher.ListProviders()
		if len(providers) == 0 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgProviderListEmpty))
			return
		}
		current := switcher.GetActiveProvider()
		var sb strings.Builder
		sb.WriteString(e.i18n.T(MsgProviderListTitle))
		for _, prov := range providers {
			marker := "  "
			if current != nil && prov.Name == current.Name {
				marker = "▶ "
			}
			detail := prov.Name
			if prov.BaseURL != "" {
				detail += " (" + prov.BaseURL + ")"
			}
			if prov.Model != "" {
				detail += " [" + prov.Model + "]"
			}
			sb.WriteString(fmt.Sprintf("%s%s\n", marker, detail))
		}
		sb.WriteString("\n" + e.i18n.T(MsgProviderSwitchHint))
		e.reply(p, msg.ReplyCtx, sb.String())

	case "add":
		e.cmdProviderAdd(p, msg, switcher, args[1:])

	case "remove", "rm", "delete":
		e.cmdProviderRemove(p, msg, switcher, args[1:])

	case "switch":
		if len(args) < 2 {
			e.reply(p, msg.ReplyCtx, "Usage: /provider switch <name>")
			return
		}
		e.switchProvider(p, msg, switcher, args[1])

	case "current":
		current := switcher.GetActiveProvider()
		if current == nil {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgProviderNone))
			return
		}
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderCurrent), current.Name))

	case "clear", "reset", "none":
		switcher.SetActiveProvider("")
		e.cleanupConversation(msg.SessionKey)
		if e.providerSaveFunc != nil {
			if err := e.providerSaveFunc(""); err != nil {
				slog.Error("failed to save provider", "error", err)
			}
		}
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgProviderCleared))

	default:
		e.switchProvider(p, msg, switcher, args[0])
	}
}

func (e *Engine) cmdProviderAdd(p Platform, msg *Message, switcher ProviderSwitcher, args []string) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgProviderAddUsage))
		return
	}

	var prov ProviderConfig

	// Join args back; detect JSON (starts with '{') vs positional
	raw := strings.Join(args, " ")
	raw = strings.TrimSpace(raw)

	if strings.HasPrefix(raw, "{") {
		// JSON format: /provider add {"name":"relay","api_key":"sk-xxx",...}
		var jp struct {
			Name    string            `json:"name"`
			APIKey  string            `json:"api_key"`
			BaseURL string            `json:"base_url"`
			Model   string            `json:"model"`
			Env     map[string]string `json:"env"`
		}
		if err := json.Unmarshal([]byte(raw), &jp); err != nil {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderAddFailed), "invalid JSON: "+err.Error()))
			return
		}
		if jp.Name == "" {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderAddFailed), "\"name\" is required"))
			return
		}
		prov = ProviderConfig{Name: jp.Name, APIKey: jp.APIKey, BaseURL: jp.BaseURL, Model: jp.Model, Env: jp.Env}
	} else {
		// Positional: /provider add <name> <api_key> [base_url] [model]
		if len(args) < 2 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgProviderAddUsage))
			return
		}
		prov.Name = args[0]
		prov.APIKey = args[1]
		if len(args) > 2 {
			prov.BaseURL = args[2]
		}
		if len(args) > 3 {
			prov.Model = args[3]
		}
	}

	// Check for duplicates
	for _, existing := range switcher.ListProviders() {
		if existing.Name == prov.Name {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderAddFailed), fmt.Sprintf("provider %q already exists", prov.Name)))
			return
		}
	}

	// Add to runtime
	updated := append(switcher.ListProviders(), prov)
	switcher.SetProviders(updated)

	// Persist to config
	if e.providerAddSaveFunc != nil {
		if err := e.providerAddSaveFunc(prov); err != nil {
			slog.Error("failed to persist provider", "error", err)
		}
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderAdded), prov.Name, prov.Name))
}

func (e *Engine) cmdProviderRemove(p Platform, msg *Message, switcher ProviderSwitcher, args []string) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, "Usage: /provider remove <name>")
		return
	}
	name := args[0]

	providers := switcher.ListProviders()
	found := false
	var remaining []ProviderConfig
	for _, prov := range providers {
		if prov.Name == name {
			found = true
		} else {
			remaining = append(remaining, prov)
		}
	}

	if !found {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderNotFound), name))
		return
	}

	// If removing the active provider, clear it
	active := switcher.GetActiveProvider()
	switcher.SetProviders(remaining)
	if active != nil && active.Name == name {
		// No active provider after removal
		slog.Info("removed active provider, clearing selection", "name", name)
	}

	// Persist
	if e.providerRemoveSaveFunc != nil {
		if err := e.providerRemoveSaveFunc(name); err != nil {
			slog.Error("failed to persist provider removal", "error", err)
		}
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderRemoved), name))
}

func (e *Engine) switchProvider(p Platform, msg *Message, switcher ProviderSwitcher, name string) {
	if !switcher.SetActiveProvider(name) {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderNotFound), name))
		return
	}
	e.cleanupConversation(msg.SessionKey)

	if e.providerSaveFunc != nil {
		if err := e.providerSaveFunc(name); err != nil {
			slog.Error("failed to save provider", "error", err)
		}
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgProviderSwitched), name))
}

// ──────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────

// SendToSession sends a message to an active session from an external caller (API/CLI).
// If sessionKey is empty, it picks the first active session.
func (e *Engine) SendToSession(sessionKey, message string) error {
	var conv *ConversationContext
	if sessionKey != "" {
		conv = e.conversations.Get(sessionKey)
	} else {
		for _, c := range e.conversations.List() {
			conv = c
			break
		}
	}

	if conv == nil {
		return fmt.Errorf("no active session found (key=%q)", sessionKey)
	}

	conv.mu.Lock()
	p := conv.ReplyPlatform
	replyCtx := conv.ReplyCtx
	conv.mu.Unlock()

	if p == nil {
		return fmt.Errorf("no active session found (key=%q)", sessionKey)
	}

	return p.Send(e.ctx, replyCtx, message)
}

// sendPermissionPrompt sends a permission prompt with interactive buttons when
// the platform supports them. Fallback chain: InlineButtonSender → CardSender → plain text.
func (e *Engine) sendPermissionPrompt(p Platform, replyCtx any, prompt, toolName, toolInput string) {
	// Try inline buttons first (Telegram)
	if bs, ok := p.(InlineButtonSender); ok {
		buttons := [][]ButtonOption{
			{
				{Text: e.i18n.T(MsgPermBtnAllow), Data: "perm:allow"},
				{Text: e.i18n.T(MsgPermBtnDeny), Data: "perm:deny"},
			},
			{
				{Text: e.i18n.T(MsgPermBtnAllowAll), Data: "perm:allow_all"},
			},
		}
		if err := bs.SendWithButtons(e.ctx, replyCtx, prompt, buttons); err == nil {
			return
		}
		slog.Warn("sendPermissionPrompt: inline buttons failed, falling back")
	}

	// Try card with buttons (Feishu/Lark)
	if supportsCards(p) {
		body := fmt.Sprintf(e.i18n.T(MsgPermCardBody), toolName, toolInput)
		extra := func(label, color string) map[string]string {
			return map[string]string{
				"perm_label": label,
				"perm_color": color,
				"perm_body":  body,
			}
		}
		allowBtn := CardButton{Text: e.i18n.T(MsgPermBtnAllow), Type: "primary", Value: "perm:allow",
			Extra: extra("✅ "+e.i18n.T(MsgPermBtnAllow), "green")}
		denyBtn := CardButton{Text: e.i18n.T(MsgPermBtnDeny), Type: "danger", Value: "perm:deny",
			Extra: extra("❌ "+e.i18n.T(MsgPermBtnDeny), "red")}
		allowAllBtn := CardButton{Text: e.i18n.T(MsgPermBtnAllowAll), Type: "default", Value: "perm:allow_all",
			Extra: extra("✅ "+e.i18n.T(MsgPermBtnAllowAll), "green")}

		card := NewCard().
			Title(e.i18n.T(MsgPermCardTitle), "orange").
			Markdown(body).
			ButtonsEqual(allowBtn, denyBtn).
			Buttons(allowAllBtn).
			Note(e.i18n.T(MsgPermCardNote)).
			Build()
		e.sendWithCard(p, replyCtx, card)
		return
	}

	e.send(p, replyCtx, prompt)
}

// sendAskQuestionPrompt renders one question (by index) from the AskUserQuestion list.
// qIdx is the 0-based index of the question to display.
func (e *Engine) sendAskQuestionPrompt(p Platform, replyCtx any, questions []UserQuestion, qIdx int) {
	if qIdx >= len(questions) {
		return
	}
	q := questions[qIdx]
	total := len(questions)

	titleSuffix := ""
	if total > 1 {
		titleSuffix = fmt.Sprintf(" (%d/%d)", qIdx+1, total)
	}

	headerText := q.Header
	if headerText == "" {
		headerText = q.Question
	}

	// Try card (Feishu/Lark)
	if supportsCards(p) {
		cb := NewCard().Title(e.i18n.T(MsgAskQuestionTitle)+titleSuffix, "blue")
		body := "**" + q.Question + "**"
		if q.MultiSelect {
			body += e.i18n.T(MsgAskQuestionMulti)
		}
		cb.Markdown(body)
		for i, opt := range q.Options {
			desc := opt.Label
			if opt.Description != "" {
				desc += " — " + opt.Description
			}
			answerData := fmt.Sprintf("askq:%d:%d", qIdx, i+1)
			cb.ListItemBtnExtra(desc, opt.Label, "default", answerData, map[string]string{
				"askq_label":    opt.Label,
				"askq_question": q.Question,
			})
		}
		cb.Note(e.i18n.T(MsgAskQuestionNote))
		e.sendWithCard(p, replyCtx, cb.Build())
		return
	}

	// Try inline buttons (Telegram)
	if bs, ok := p.(InlineButtonSender); ok {
		var textBuf strings.Builder
		textBuf.WriteString("❓ *")
		textBuf.WriteString(q.Question)
		textBuf.WriteString("*")
		textBuf.WriteString(titleSuffix)
		if q.MultiSelect {
			textBuf.WriteString(e.i18n.T(MsgAskQuestionMulti))
		}
		hasDesc := false
		for _, opt := range q.Options {
			if opt.Description != "" {
				hasDesc = true
				break
			}
		}
		if hasDesc {
			textBuf.WriteString("\n")
			for i, opt := range q.Options {
				textBuf.WriteString(fmt.Sprintf("\n*%d. %s*", i+1, opt.Label))
				if opt.Description != "" {
					textBuf.WriteString(" — ")
					textBuf.WriteString(opt.Description)
				}
			}
			textBuf.WriteString("\n")
		}
		var rows [][]ButtonOption
		for i, opt := range q.Options {
			rows = append(rows, []ButtonOption{{Text: opt.Label, Data: fmt.Sprintf("askq:%d:%d", qIdx, i+1)}})
		}
		if err := bs.SendWithButtons(e.ctx, replyCtx, textBuf.String(), rows); err == nil {
			return
		}
	}

	// Plain text fallback
	var sb strings.Builder
	sb.WriteString("❓ **")
	sb.WriteString(q.Question)
	sb.WriteString("**")
	sb.WriteString(titleSuffix)
	if q.MultiSelect {
		sb.WriteString(e.i18n.T(MsgAskQuestionMulti))
	}
	sb.WriteString("\n\n")
	for i, opt := range q.Options {
		sb.WriteString(fmt.Sprintf("%d. **%s**", i+1, opt.Label))
		if opt.Description != "" {
			sb.WriteString(" — ")
			sb.WriteString(opt.Description)
		}
		sb.WriteString("\n")
	}
	sb.WriteString(fmt.Sprintf("\n%s", e.i18n.T(MsgAskQuestionNote)))
	e.send(p, replyCtx, sb.String())
}

// send wraps p.Send with error logging and slow-operation warnings.
func (e *Engine) send(p Platform, replyCtx any, content string) {
	start := time.Now()
	if err := p.Send(e.ctx, replyCtx, content); err != nil {
		slog.Error("platform send failed", "platform", p.Name(), "error", err, "content_len", len(content))
	}
	if elapsed := time.Since(start); elapsed >= slowPlatformSend {
		slog.Warn("slow platform send", "platform", p.Name(), "elapsed", elapsed, "content_len", len(content))
	}
}

// drainEvents discards any buffered events from the channel.
// Called before a new turn to prevent stale events from a previous turn's
// agent process from being mistaken for the new turn's response.
func drainEvents(ch <-chan Event) {
	drained := 0
	for {
		select {
		case <-ch:
			drained++
		default:
			if drained > 0 {
				slog.Warn("drained stale events from previous turn", "count", drained)
			}
			return
		}
	}
}

// reply wraps p.Reply with error logging and slow-operation warnings.
func (e *Engine) reply(p Platform, replyCtx any, content string) {
	start := time.Now()
	if err := p.Reply(e.ctx, replyCtx, content); err != nil {
		slog.Error("platform reply failed", "platform", p.Name(), "error", err, "content_len", len(content))
	}
	if elapsed := time.Since(start); elapsed >= slowPlatformSend {
		slog.Warn("slow platform reply", "platform", p.Name(), "elapsed", elapsed, "content_len", len(content))
	}
}

// replyWithButtons sends a reply with inline buttons if the platform supports it,
// otherwise falls back to plain text reply.
func (e *Engine) replyWithButtons(p Platform, replyCtx any, content string, buttons [][]ButtonOption) {
	if bs, ok := p.(InlineButtonSender); ok {
		if err := bs.SendWithButtons(e.ctx, replyCtx, content, buttons); err == nil {
			return
		}
	}
	e.reply(p, replyCtx, content)
}

func isInlineButtonOnlyPlatform(p Platform) bool {
	if _, ok := p.(InlineButtonSender); !ok {
		return false
	}
	return !supportsCards(p)
}

func supportsCards(p Platform) bool {
	_, ok := p.(CardSender)
	return ok
}

// replyWithCard sends a structured card via CardSender.
// For platforms without card support, renders as plain text (no intermediate fallback).
func (e *Engine) replyWithCard(p Platform, replyCtx any, card *Card) {
	if card == nil {
		slog.Error("replyWithCard: nil card", "platform", p.Name())
		return
	}
	if cs, ok := p.(CardSender); ok {
		if err := cs.ReplyCard(e.ctx, replyCtx, card); err != nil {
			slog.Error("card reply failed", "platform", p.Name(), "error", err)
		}
		return
	}
	e.reply(p, replyCtx, card.RenderText())
}

// sendWithCard sends a card as a new message (not a reply).
func (e *Engine) sendWithCard(p Platform, replyCtx any, card *Card) {
	if card == nil {
		slog.Error("sendWithCard: nil card", "platform", p.Name())
		return
	}
	if cs, ok := p.(CardSender); ok {
		if err := cs.SendCard(e.ctx, replyCtx, card); err != nil {
			slog.Error("card send failed", "platform", p.Name(), "error", err)
		}
		return
	}
	e.send(p, replyCtx, card.RenderText())
}


