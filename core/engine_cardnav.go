package core

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ──────────────────────────────────────────────────────────────
// Card navigation (in-place card updates)
// ──────────────────────────────────────────────────────────────

// registerCardHandlers registers all nav: and act: card handlers into the
// CardService registry. This replaces the monolithic switch/case approach
// with a registry-based dispatch that is easier to extend and test.
func (e *Engine) registerCardHandlers() {
	cs := e.cardService

	// ── Nav handlers ────────────────────────────────────────
	cs.RegisterNav("/help", func(args, sk string) *Card {
		return e.renderHelpGroupCard(args)
	})
	cs.RegisterNav("/model", func(args, sk string) *Card {
		return e.renderModelCard()
	})
	cs.RegisterNav("/reasoning", func(args, sk string) *Card {
		return e.renderReasoningCard()
	})
	cs.RegisterNav("/mode", func(args, sk string) *Card {
		return e.renderModeCard()
	})
	cs.RegisterNav("/lang", func(args, sk string) *Card {
		return e.renderLangCard()
	})
	cs.RegisterNav("/status", func(args, sk string) *Card {
		return e.renderStatusCard(sk)
	})
	cs.RegisterNav("/list", func(args, sk string) *Card {
		page := 1
		if args != "" {
			if n, err := strconv.Atoi(args); err == nil && n > 0 {
				page = n
			}
		}
		return e.renderListCardSafe(sk, page)
	})
	cs.RegisterNav("/current", func(args, sk string) *Card {
		return e.renderCurrentCard(sk)
	})
	cs.RegisterNav("/history", func(args, sk string) *Card {
		return e.renderHistoryCard(sk)
	})
	cs.RegisterNav("/provider", func(args, sk string) *Card {
		return e.renderProviderCard()
	})
	cs.RegisterNav("/cron", func(args, sk string) *Card {
		return e.renderCronCard(sk)
	})
	cs.RegisterNav("/commands", func(args, sk string) *Card {
		return e.renderCommandsCard()
	})
	cs.RegisterNav("/alias", func(args, sk string) *Card {
		return e.renderAliasCard()
	})
	cs.RegisterNav("/config", func(args, sk string) *Card {
		return e.renderConfigCard()
	})
	cs.RegisterNav("/skills", func(args, sk string) *Card {
		return e.renderSkillsCard()
	})
	cs.RegisterNav("/doctor", func(args, sk string) *Card {
		return e.renderDoctorCard()
	})
	cs.RegisterNav("/version", func(args, sk string) *Card {
		return e.renderVersionCard()
	})
	cs.RegisterNav("/new", func(args, sk string) *Card {
		return e.renderCurrentCard(sk)
	})
	cs.RegisterNav("/quiet", func(args, sk string) *Card {
		return e.renderStatusCard(sk)
	})
	cs.RegisterNav("/switch", func(args, sk string) *Card {
		return e.renderListCardSafe(sk, 1)
	})
	cs.RegisterNav("/delete-mode", func(args, sk string) *Card {
		if strings.HasPrefix(args, "cancel") {
			return e.renderListCardSafe(sk, 1)
		}
		return e.renderDeleteModeCard(sk)
	})
	cs.RegisterNav("/stop", func(args, sk string) *Card {
		return e.renderStatusCard(sk)
	})
	cs.RegisterNav("/upgrade", func(args, sk string) *Card {
		return e.renderUpgradeCard()
	})

	// ── Act handlers (combined execute + render) ────────────
	cs.RegisterAct("/model", func(args, sk string) *Card {
		return e.actAndRenderModel(args, sk)
	})
	cs.RegisterAct("/switch", func(args, sk string) *Card {
		return e.actAndRenderSwitch(args, sk)
	})
}

// handleCardNav is called by platforms that support in-place card updates.
// It routes nav: and act: prefixed actions to the appropriate render function.
//
// The routing first checks the CardService registry for a matching handler.
// If no handler is found, it falls back to the legacy switch/case dispatch.
func (e *Engine) handleCardNav(action string, sessionKey string) *Card {
	var prefix, body string
	if i := strings.Index(action, ":"); i >= 0 {
		prefix = action[:i]
		body = action[i+1:]
	} else {
		return nil
	}

	cmd, args := body, ""
	if i := strings.IndexByte(body, ' '); i >= 0 {
		cmd = body[:i]
		args = strings.TrimSpace(body[i+1:])
	}

	// ── Registry-based dispatch (preferred) ─────────────────
	if prefix == "act" {
		if card := e.cardService.HandleAct(cmd, args, sessionKey); card != nil {
			return card
		}
		// Fallback: act commands that only have side-effects, then render via nav
		e.executeCardAction(cmd, args, sessionKey)
	}

	// Approval workflow: handle approve/reject button clicks from admin card
	if strings.HasPrefix(cmd, "approval/") {
		approvalAction := strings.TrimPrefix(cmd, "approval/")
		if args != "" {
			approvalAction += " " + args
		}
		return e.handleApprovalCardAction(approvalAction, sessionKey)
	}

	if card := e.cardService.HandleNav(cmd, args, sessionKey); card != nil {
		return card
	}

	// ── Legacy fallback (will be removed once all handlers are registered) ──
	switch cmd {
	case "/help":
		return e.renderHelpGroupCard(args)
	case "/model":
		return e.renderModelCard()
	case "/reasoning":
		return e.renderReasoningCard()
	case "/mode":
		return e.renderModeCard()
	case "/lang":
		return e.renderLangCard()
	case "/status":
		return e.renderStatusCard(sessionKey)
	case "/list":
		page := 1
		if args != "" {
			if n, err := strconv.Atoi(args); err == nil && n > 0 {
				page = n
			}
		}
		return e.renderListCardSafe(sessionKey, page)
	case "/current":
		return e.renderCurrentCard(sessionKey)
	case "/history":
		return e.renderHistoryCard(sessionKey)
	case "/provider":
		return e.renderProviderCard()
	case "/cron":
		return e.renderCronCard(sessionKey)
	case "/commands":
		return e.renderCommandsCard()
	case "/alias":
		return e.renderAliasCard()
	case "/config":
		return e.renderConfigCard()
	case "/skills":
		return e.renderSkillsCard()
	case "/doctor":
		return e.renderDoctorCard()
	case "/version":
		return e.renderVersionCard()
	case "/new":
		return e.renderCurrentCard(sessionKey)
	case "/quiet":
		return e.renderStatusCard(sessionKey)
	case "/switch":
		return e.renderListCardSafe(sessionKey, 1)
	case "/delete-mode":
		if strings.HasPrefix(args, "cancel") {
			return e.renderListCardSafe(sessionKey, 1)
		}
		return e.renderDeleteModeCard(sessionKey)
	case "/stop":
		return e.renderStatusCard(sessionKey)
	case "/upgrade":
		return e.renderUpgradeCard()
	}
	return nil
}

// actAndRenderModel performs model switching and renders the model card in a
// single pass, fetching AvailableModels only once. This avoids the double-fetch
// that previously caused Feishu card callback timeouts.
func (e *Engine) actAndRenderModel(args, sessionKey string) *Card {
	switcher, ok := e.agent.(ModelSwitcher)
	if !ok {
		return e.simpleCard(e.i18n.T(MsgCardTitleModel), "indigo", e.i18n.T(MsgModelNotSupported))
	}

	fetchCtx, cancel := context.WithTimeout(e.ctx, 3*time.Second)
	defer cancel()
	models := switcher.AvailableModels(fetchCtx)

	if args != "" {
		target := args
		if idx, err := strconv.Atoi(target); err == nil && idx >= 1 && idx <= len(models) {
			target = models[idx-1].Name
		}
		switcher.SetModel(target)
		e.cleanupConversation(sessionKey)
		conv := e.conversations.GetOrCreate(sessionKey)
		conv.SetAgentSessionID("")
		conv.ClearHistory()
		e.conversations.Save()
	}

	return e.renderModelCardWithModels(models)
}

// actAndRenderSwitch performs session switching and renders the session list card
// in a single pass, fetching ListSessions only once.
func (e *Engine) actAndRenderSwitch(args, sessionKey string) *Card {
	agent := e.sessionContextForKey(sessionKey)

	if args == "" {
		return e.renderListCardSafe(sessionKey, 1)
	}

	agentSessions, err := agent.ListSessions(e.ctx)
	if err != nil {
		return e.simpleCard(e.i18n.Tf(MsgCardTitleSessions, agent.Name(), 0), "red", err.Error())
	}
	if len(agentSessions) == 0 {
		return e.simpleCard(e.i18n.Tf(MsgCardTitleSessions, agent.Name(), 0), "turquoise", e.i18n.T(MsgListEmpty))
	}

	matched := e.matchSession(agentSessions, args)
	if matched != nil {
		e.cleanupConversation(sessionKey)
		conv := e.conversations.GetOrCreate(sessionKey)
		conv.SetAgentInfo(matched.ID, matched.Summary)
		conv.ClearHistory()
		e.conversations.Save()
	}

	return e.renderListCardWithSessions(sessionKey, 1, agent, agentSessions)
}

// executeCardAction performs the side-effect for act: prefixed actions
// (e.g. switching model/mode/lang) before the card is re-rendered.
func (e *Engine) executeCardAction(cmd, args, sessionKey string) {
	switch cmd {
	case "/model":
		if args == "" {
			return
		}
		switcher, ok := e.agent.(ModelSwitcher)
		if !ok {
			return
		}
		fetchCtx, cancel := context.WithTimeout(e.ctx, 3*time.Second)
		defer cancel()
		models := switcher.AvailableModels(fetchCtx)
		target := args
		if idx, err := strconv.Atoi(target); err == nil && idx >= 1 && idx <= len(models) {
			target = models[idx-1].Name
		}
		switcher.SetModel(target)
		e.cleanupConversation(sessionKey)
		conv := e.conversations.GetOrCreate(sessionKey)
		conv.SetAgentSessionID("")
		conv.ClearHistory()
		e.conversations.Save()

	case "/reasoning":
		if args == "" {
			return
		}
		switcher, ok := e.agent.(ReasoningEffortSwitcher)
		if !ok {
			return
		}
		efforts := switcher.AvailableReasoningEfforts()
		target := strings.ToLower(strings.TrimSpace(args))
		if idx, err := strconv.Atoi(target); err == nil && idx >= 1 && idx <= len(efforts) {
			target = efforts[idx-1]
		}
		for _, effort := range efforts {
			if effort == target {
				switcher.SetReasoningEffort(target)
				e.cleanupConversation(sessionKey)
				conv := e.conversations.GetOrCreate(sessionKey)
				conv.SetAgentSessionID("")
				conv.ClearHistory()
				e.conversations.Save()
				return
			}
		}

	case "/mode":
		if args == "" {
			return
		}
		switcher, ok := e.agent.(ModeSwitcher)
		if !ok {
			return
		}
		switcher.SetMode(strings.ToLower(args))
		e.cleanupConversation(sessionKey)

	case "/lang":
		if args == "" {
			return
		}
		target := strings.ToLower(strings.TrimSpace(args))
		var lang Language
		switch target {
		case "en", "english":
			lang = LangEnglish
		case "zh", "cn", "chinese":
			lang = LangChinese
		case "zh-tw", "zh_tw", "zhtw":
			lang = LangTraditionalChinese
		case "ja", "jp", "japanese":
			lang = LangJapanese
		case "es", "spanish":
			lang = LangSpanish
		case "auto":
			lang = LangAuto
		default:
			return
		}
		e.i18n.SetLang(lang)

	case "/provider":
		if args == "" {
			return
		}
		switcher, ok := e.agent.(ProviderSwitcher)
		if !ok {
			return
		}
		if switcher.SetActiveProvider(args) {
			e.cleanupConversation(sessionKey)
			if e.providerSaveFunc != nil {
				_ = e.providerSaveFunc(args)
			}
		}

	case "/new":
		e.cleanupConversation(sessionKey)
		conv := e.conversations.GetOrCreate(sessionKey)
		conv.NewSession("")

	case "/delete-mode":
		e.executeDeleteModeAction(sessionKey, args)

	case "/quiet":
		conv := e.conversations.GetOrCreate(sessionKey)
		conv.mu.Lock()
		conv.Quiet = !conv.Quiet
		conv.mu.Unlock()

	case "/switch":
		if args == "" {
			return
		}
		agent := e.sessionContextForKey(sessionKey)
		agentSessions, err := agent.ListSessions(e.ctx)
		if err != nil || len(agentSessions) == 0 {
			return
		}
		matched := e.matchSession(agentSessions, args)
		if matched == nil {
			return
		}
		e.cleanupConversation(sessionKey)
		conv := e.conversations.GetOrCreate(sessionKey)
		conv.SetAgentInfo(matched.ID, matched.Summary)
		conv.ClearHistory()
		e.conversations.Save()

	case "/stop":
		conv := e.conversations.Get(sessionKey)
		if conv == nil {
			return
		}
		conv.mu.Lock()
		pending := conv.PendingPerm
		quietMode := conv.Quiet
		agentSession := conv.AgentSession
		conv.mu.Unlock()
		conv.ClearRuntime()
		if quietMode {
			conv.mu.Lock()
			conv.Quiet = true
			conv.mu.Unlock()
		}
		if pending != nil {
			pending.resolve()
		}
		if agentSession != nil {
			slog.Debug("cleanupConversation: closing agent session", "session", sessionKey)
			go agentSession.Close()
		}
	}
}

func (e *Engine) getOrCreateDeleteModeState(sessionKey string, p Platform, replyCtx any) *deleteModeState {
	conv := e.conversations.GetOrCreate(sessionKey)
	conv.mu.Lock()
	defer conv.mu.Unlock()
	conv.ReplyPlatform = p
	conv.ReplyCtx = replyCtx
	if conv.DeleteMode == nil {
		conv.DeleteMode = &deleteModeState{}
	}
	dm := conv.DeleteMode
	dm.page = 1
	dm.phase = "select"
	dm.hint = ""
	dm.result = ""
	dm.selectedIDs = make(map[string]struct{})
	return dm
}

func (e *Engine) getDeleteModeState(sessionKey string) *deleteModeState {
	conv := e.conversations.Get(sessionKey)
	if conv == nil {
		return nil
	}
	conv.mu.Lock()
	defer conv.mu.Unlock()
	if conv.DeleteMode == nil {
		return nil
	}
	cp := &deleteModeState{
		page:        conv.DeleteMode.page,
		selectedIDs: make(map[string]struct{}, len(conv.DeleteMode.selectedIDs)),
		phase:       conv.DeleteMode.phase,
		hint:        conv.DeleteMode.hint,
		result:      conv.DeleteMode.result,
	}
	for id := range conv.DeleteMode.selectedIDs {
		cp.selectedIDs[id] = struct{}{}
	}
	return cp
}

func (e *Engine) clearDeleteModeState(sessionKey string) {
	conv := e.conversations.Get(sessionKey)
	if conv == nil {
		return
	}
	conv.mu.Lock()
	conv.DeleteMode = nil
	conv.mu.Unlock()
}

func (e *Engine) renderDeleteModeCard(sessionKey string) *Card {
	agent := e.sessionContextForKey(sessionKey)
	agentSessions, err := agent.ListSessions(e.ctx)
	if err != nil {
		return e.simpleCard(e.i18n.T(MsgDeleteModeTitle), "red", err.Error())
	}
	dm := e.getDeleteModeState(sessionKey)
	if dm == nil {
		return e.simpleCard(e.i18n.T(MsgDeleteModeTitle), "red", e.i18n.T(MsgDeleteUsage))
	}
	switch dm.phase {
	case "confirm":
		return e.renderDeleteModeConfirmCard(dm, agentSessions)
	case "result":
		return e.renderDeleteModeResultCard(dm)
	default:
		return e.renderDeleteModeSelectCard(sessionKey, dm, agentSessions)
	}
}

func (e *Engine) renderDeleteModeSelectCard(sessionKey string, dm *deleteModeState, agentSessions []AgentSessionInfo) *Card {
	if len(agentSessions) == 0 {
		return e.simpleCard(e.i18n.T(MsgDeleteModeTitle), "red", e.i18n.T(MsgListEmpty))
	}
	total := len(agentSessions)
	totalPages := (total + listPageSize - 1) / listPageSize
	page := dm.page
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * listPageSize
	end := start + listPageSize
	if end > total {
		end = total
	}

	cb := NewCard().Title(e.i18n.T(MsgDeleteModeTitle), "carmine")
	conv := e.conversations.GetOrCreate(sessionKey)
	conv.mu.Lock()
	activeAgentID := conv.AgentSessionID
	conv.mu.Unlock()
	selectedCount := 0
	for i := start; i < end; i++ {
		s := agentSessions[i]
		isActive := activeAgentID == s.ID
		isSelected := false
		if !isActive {
			_, isSelected = dm.selectedIDs[s.ID]
		}
		marker := "◻"
		if isActive {
			marker = "▶"
		} else if isSelected {
			marker = "☑"
			selectedCount++
		}
		btnText := e.i18n.T(MsgDeleteModeSelect)
		btnType := "default"
		action := fmt.Sprintf("act:/delete-mode toggle %s", s.ID)
		if isActive {
			btnText = e.i18n.T(MsgCardTitleCurrentSession)
			btnType = "primary"
			action = fmt.Sprintf("act:/delete-mode noop %s", s.ID)
		} else if isSelected {
			btnText = e.i18n.T(MsgDeleteModeSelected)
			btnType = "primary"
		}
		cb.ListItemBtn(
			e.i18n.Tf(MsgListItem, marker, i+1, e.deleteSessionDisplayName(&s), s.MessageCount, s.ModifiedAt.Format("01-02 15:04")),
			btnText,
			btnType,
			action,
		)
	}
	cb.TaggedNote("delete-mode-selected-count", e.i18n.Tf(MsgDeleteModeSelectedCount, selectedCount))
	if dm.hint != "" {
		cb.Note(dm.hint)
	}
	cb.Buttons(
		DangerBtn(e.i18n.T(MsgDeleteModeDeleteSelected), "act:/delete-mode confirm"),
		DefaultBtn(e.i18n.T(MsgDeleteModeCancel), "act:/delete-mode cancel"),
	)

	var navBtns []CardButton
	if page > 1 {
		navBtns = append(navBtns, DefaultBtn(e.i18n.T(MsgCardPrev), fmt.Sprintf("act:/delete-mode page %d", page-1)))
	}
	if page < totalPages {
		navBtns = append(navBtns, DefaultBtn(e.i18n.T(MsgCardNext), fmt.Sprintf("act:/delete-mode page %d", page+1)))
	}
	if len(navBtns) > 0 {
		cb.Buttons(navBtns...)
	}
	return cb.Build()
}

func (e *Engine) renderDeleteModeConfirmCard(dm *deleteModeState, agentSessions []AgentSessionInfo) *Card {
	selectedNames := e.deleteModeSelectionNames(dm, agentSessions)
	body := strings.Join(selectedNames, "\n")
	if body == "" {
		body = e.i18n.T(MsgDeleteModeEmptySelection)
	}
	
	// Create the cmd string with the selected IDs
	var idList []string
	for id := range dm.selectedIDs {
		// Use only the ID string for the command argument
		idList = append(idList, id)
	}
	cmdArgs := strings.Join(idList, ",")
	submitAction := "cmd:/delete " + cmdArgs

	return NewCard().
		Title(e.i18n.T(MsgDeleteModeConfirmTitle), "carmine").
		Markdown(body).
		Buttons(
			DangerBtn(e.i18n.T(MsgDeleteModeConfirmButton), submitAction),
			DefaultBtn(e.i18n.T(MsgDeleteModeBackButton), "act:/delete-mode back"),
		).
		Build()
}

func (e *Engine) renderDeleteModeResultCard(dm *deleteModeState) *Card {
	return NewCard().
		Title(e.i18n.T(MsgDeleteModeResultTitle), "turquoise").
		Markdown(dm.result).
		Buttons(DefaultBtn(e.i18n.T(MsgCardBack), "nav:/list 1")).
		Build()
}

func (e *Engine) deleteModeSelectionNames(dm *deleteModeState, agentSessions []AgentSessionInfo) []string {
	names := make([]string, 0, len(dm.selectedIDs))
	for i := range agentSessions {
		if _, ok := dm.selectedIDs[agentSessions[i].ID]; ok {
			names = append(names, "- "+e.deleteSessionDisplayName(&agentSessions[i]))
		}
	}
	return names
}

func (e *Engine) executeDeleteModeAction(sessionKey, args string) {
	conv := e.conversations.Get(sessionKey)
	if conv == nil {
		return
	}

	fields := strings.Fields(args)
	if len(fields) == 0 {
		return
	}

	conv.mu.Lock()
	defer conv.mu.Unlock()
	if conv.DeleteMode == nil {
		return
	}

	dm := conv.DeleteMode
	switch fields[0] {
	case "toggle":
		if len(fields) < 2 {
			return
		}
		id := fields[1]
		if _, ok := dm.selectedIDs[id]; ok {
			delete(dm.selectedIDs, id)
		} else {
			dm.selectedIDs[id] = struct{}{}
		}
		dm.phase = "select"
		dm.hint = ""
	case "page":
		if len(fields) < 2 {
			return
		}
		if n, err := strconv.Atoi(fields[1]); err == nil && n > 0 {
			dm.page = n
		}
		dm.phase = "select"
	case "confirm":
		if len(dm.selectedIDs) == 0 {
			dm.phase = "select"
			dm.hint = e.i18n.T(MsgDeleteModeEmptySelection)
			return
		}
		dm.phase = "confirm"
		dm.hint = ""
	case "back":
		dm.phase = "select"
	case "submit":
		lines := e.submitDeleteModeSelection(sessionKey, dm)
		dm.selectedIDs = make(map[string]struct{})
		dm.result = strings.Join(lines, "\n")
		dm.hint = ""
		dm.phase = "result"
	case "form-submit":
		dm.selectedIDs = parseDeleteModeSelectedIDs(fields[1:])
		if len(dm.selectedIDs) == 0 {
			dm.phase = "select"
			dm.hint = e.i18n.T(MsgDeleteModeEmptySelection)
			return
		}
		dm.phase = "confirm"
		dm.hint = ""
	case "cancel":
		conv.DeleteMode = nil
	}
}

func parseDeleteModeSelectedIDs(args []string) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, arg := range args {
		for _, id := range strings.Split(arg, ",") {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			ids[id] = struct{}{}
		}
	}
	return ids
}

func (e *Engine) submitDeleteModeSelection(sessionKey string, dm *deleteModeState) []string {
	agent := e.sessionContextForKey(sessionKey)
	deleter, ok := agent.(SessionDeleter)
	if !ok {
		return []string{e.i18n.T(MsgDeleteNotSupported)}
	}
	agentSessions, err := agent.ListSessions(e.ctx)
	if err != nil {
		return []string{fmt.Sprintf("❌ %v", err)}
	}
	seen := make(map[string]struct{}, len(agentSessions))
	lines := make([]string, 0, len(dm.selectedIDs))
	for i := range agentSessions {
		seen[agentSessions[i].ID] = struct{}{}
		if _, ok := dm.selectedIDs[agentSessions[i].ID]; !ok {
			continue
		}
		if line := e.deleteSingleSessionReply(&Message{SessionKey: sessionKey}, deleter, &agentSessions[i]); line != "" {
			lines = append(lines, line)
		}
	}
	missingIDs := make([]string, 0)
	for id := range dm.selectedIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		missingIDs = append(missingIDs, id)
	}
	sort.Strings(missingIDs)
	for _, id := range missingIDs {
		lines = append(lines, fmt.Sprintf(e.i18n.T(MsgDeleteModeMissingSession), id))
	}
	if len(lines) == 0 {
		lines = append(lines, e.i18n.T(MsgDeleteModeEmptySelection))
	}
	return lines
}

func (e *Engine) renderLangCard() *Card {
	cur := e.i18n.CurrentLang()
	name := langDisplayName(cur)

	langs := []struct{ code, label string }{
		{"en", "English"}, {"zh", "中文"}, {"zh-TW", "繁體中文"},
		{"ja", "日本語"}, {"es", "Español"}, {"auto", "Auto"},
	}
	var opts []CardSelectOption
	initVal := ""
	for _, l := range langs {
		opts = append(opts, CardSelectOption{Text: l.label, Value: "act:/lang " + l.code})
		if string(cur) == l.code || (cur == LangAuto && l.code == "auto") {
			initVal = "act:/lang " + l.code
		}
	}

	return NewCard().
		Title(e.i18n.T(MsgCardTitleLanguage), "wathet").
		Markdown(e.i18n.Tf(MsgLangCurrent, name)).
		Select(e.i18n.T(MsgLangSelectPlaceholder), opts, initVal).
		Buttons(e.cardBackButton()).
		Build()
}

func (e *Engine) renderModelCard() *Card {
	switcher, ok := e.agent.(ModelSwitcher)
	if !ok {
		return e.simpleCard(e.i18n.T(MsgCardTitleModel), "indigo", e.i18n.T(MsgModelNotSupported))
	}

	fetchCtx, cancel := context.WithTimeout(e.ctx, 3*time.Second)
	defer cancel()
	models := switcher.AvailableModels(fetchCtx)
	return e.renderModelCardWithModels(models)
}

func (e *Engine) renderModelCardWithModels(models []ModelOption) *Card {
	switcher, ok := e.agent.(ModelSwitcher)
	if !ok {
		return e.simpleCard(e.i18n.T(MsgCardTitleModel), "indigo", e.i18n.T(MsgModelNotSupported))
	}
	current := switcher.GetModel()

	var sb strings.Builder
	if current == "" {
		sb.WriteString(e.i18n.T(MsgModelDefault))
	} else {
		sb.WriteString(e.i18n.Tf(MsgModelCurrent, current))
	}

	var opts []CardSelectOption
	initVal := ""
	for i, m := range models {
		label := m.Name
		if m.Desc != "" {
			label += " — " + m.Desc
		}
		val := fmt.Sprintf("act:/model %d", i+1)
		opts = append(opts, CardSelectOption{Text: label, Value: val})
		if m.Name == current {
			initVal = val
		}
	}

	cb := NewCard().Title(e.i18n.T(MsgCardTitleModel), "indigo").
		Markdown(sb.String()).
		Select(e.i18n.T(MsgModelSelectPlaceholder), opts, initVal).
		Buttons(e.cardBackButton())
	cb.Note(e.i18n.T(MsgModelUsage))
	return cb.Build()
}

func (e *Engine) renderReasoningCard() *Card {
	switcher, ok := e.agent.(ReasoningEffortSwitcher)
	if !ok {
		return e.simpleCard(e.i18n.T(MsgCardTitleReasoning), "orange", e.i18n.T(MsgReasoningNotSupported))
	}

	efforts := switcher.AvailableReasoningEfforts()
	current := switcher.GetReasoningEffort()

	var sb strings.Builder
	if current == "" {
		sb.WriteString(e.i18n.T(MsgReasoningDefault))
	} else {
		sb.WriteString(e.i18n.Tf(MsgReasoningCurrent, current))
	}

	var opts []CardSelectOption
	initVal := ""
	for i, effort := range efforts {
		val := fmt.Sprintf("act:/reasoning %d", i+1)
		opts = append(opts, CardSelectOption{Text: effort, Value: val})
		if effort == current {
			initVal = val
		}
	}

	cb := NewCard().Title(e.i18n.T(MsgCardTitleReasoning), "orange").
		Markdown(sb.String()).
		Select(e.i18n.T(MsgReasoningSelectPlaceholder), opts, initVal).
		Buttons(e.cardBackButton())
	cb.Note(e.i18n.T(MsgReasoningUsage))
	return cb.Build()
}

func (e *Engine) renderModeCard() *Card {
	switcher, ok := e.agent.(ModeSwitcher)
	if !ok {
		return e.simpleCard(e.i18n.T(MsgCardTitleMode), "violet", e.i18n.T(MsgModeNotSupported))
	}

	current := switcher.GetMode()
	modes := switcher.PermissionModes()
	zhLike := e.i18n.IsZhLike()

	var sb strings.Builder
	for _, m := range modes {
		marker := "◻"
		if m.Key == current {
			marker = "▶"
		}
		if zhLike {
			sb.WriteString(fmt.Sprintf("%s **%s** — %s\n", marker, m.NameZh, m.DescZh))
		} else {
			sb.WriteString(fmt.Sprintf("%s **%s** — %s\n", marker, m.Name, m.Desc))
		}
	}

	var opts []CardSelectOption
	initVal := ""
	for _, m := range modes {
		label := m.Name
		if zhLike {
			label = m.NameZh
		}
		val := "act:/mode " + m.Key
		opts = append(opts, CardSelectOption{Text: label, Value: val})
		if m.Key == current {
			initVal = val
		}
	}

	cb := NewCard().Title(e.i18n.T(MsgCardTitleMode), "violet").
		Markdown(sb.String()).
		Select(e.i18n.T(MsgModeSelectPlaceholder), opts, initVal).
		Buttons(e.cardBackButton())
	cb.Note(e.i18n.T(MsgModeUsage))
	return cb.Build()
}

func (e *Engine) renderListCard(sessionKey string, page int) (*Card, error) {
	agent := e.sessionContextForKey(sessionKey)
	agentSessions, err := agent.ListSessions(e.ctx)
	if err != nil {
		return nil, fmt.Errorf(e.i18n.T(MsgListError), err)
	}
	return e.renderListCardWithSessions(sessionKey, page, agent, agentSessions), nil
}

func (e *Engine) renderListCardWithSessions(sessionKey string, page int, agent Agent, agentSessions []AgentSessionInfo) *Card {
	if len(agentSessions) == 0 {
		return e.simpleCard(e.i18n.Tf(MsgCardTitleSessions, agent.Name(), 0), "turquoise", e.i18n.T(MsgListEmpty))
	}

	total := len(agentSessions)
	totalPages := (total + listPageSize - 1) / listPageSize
	if page > totalPages {
		page = totalPages
	}

	start := (page - 1) * listPageSize
	end := start + listPageSize
	if end > total {
		end = total
	}

	agentName := agent.Name()
	conv := e.conversations.GetOrCreate(sessionKey)
	conv.mu.Lock()
	activeAgentID := conv.AgentSessionID
	conv.mu.Unlock()

	var titleStr string
	if totalPages > 1 {
		titleStr = e.i18n.Tf(MsgCardTitleSessionsPaged, agentName, total, page, totalPages)
	} else {
		titleStr = e.i18n.Tf(MsgCardTitleSessions, agentName, total)
	}

	cb := NewCard().Title(titleStr, "turquoise")
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
				displayName = e.i18n.T(MsgListEmptySummary)
			}
			if len([]rune(displayName)) > 40 {
				displayName = string([]rune(displayName)[:40]) + "…"
			}
		}
		btnType := "default"
		if s.ID == activeAgentID {
			btnType = "primary"
		}
		cb.ListItemBtn(
			e.i18n.Tf(MsgListItem, marker, i+1, displayName, s.MessageCount, s.ModifiedAt.Format("01-02 15:04")),
			fmt.Sprintf("#%d", i+1),
			btnType,
			fmt.Sprintf("act:/switch %d", i+1),
		)
	}

	var navBtns []CardButton
	if page > 1 {
		navBtns = append(navBtns, e.cardPrevButton(fmt.Sprintf("nav:/list %d", page-1)))
	}
	navBtns = append(navBtns, e.cardBackButton())
	if page < totalPages {
		navBtns = append(navBtns, e.cardNextButton(fmt.Sprintf("nav:/list %d", page+1)))
	}
	cb.Buttons(navBtns...)

	if totalPages > 1 {
		cb.Note(fmt.Sprintf(e.i18n.T(MsgListPageHint), page, totalPages))
	}

	return cb.Build()
}

// ──────────────────────────────────────────────────────────────
// Navigable sub-cards (for in-place card updates)
// ──────────────────────────────────────────────────────────────

func (e *Engine) renderCurrentCard(sessionKey string) *Card {
	conv := e.conversations.GetOrCreate(sessionKey)
	conv.mu.Lock()
	agentID := conv.AgentSessionID
	name := conv.Name
	histLen := len(conv.History)
	conv.mu.Unlock()
	if agentID == "" {
		agentID = e.i18n.T(MsgSessionNotStarted)
	}
	content := fmt.Sprintf(e.i18n.T(MsgCurrentSession), name, agentID, histLen)
	return NewCard().
		Title(e.i18n.T(MsgCardTitleCurrentSession), "turquoise").
		Markdown(content).
		Buttons(e.cardBackButton()).
		Build()
}

func (e *Engine) renderHistoryCard(sessionKey string) *Card {
	agent := e.sessionContextForKey(sessionKey)
	conv := e.conversations.GetOrCreate(sessionKey)
	entries := conv.GetHistory(10)

	conv.mu.Lock()
	agentID := conv.AgentSessionID
	conv.mu.Unlock()

	if len(entries) == 0 && agentID != "" {
		if hp, ok := agent.(HistoryProvider); ok {
			if agentEntries, err := hp.GetSessionHistory(e.ctx, agentID, 10); err == nil {
				entries = agentEntries
			}
		}
	}

	if len(entries) == 0 {
		return e.simpleCard(e.i18n.T(MsgCardTitleHistory), "turquoise", e.i18n.T(MsgHistoryEmpty))
	}

	var sb strings.Builder
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

	return NewCard().
		Title(e.i18n.Tf(MsgCardTitleHistoryLast, len(entries)), "turquoise").
		Markdown(sb.String()).
		Buttons(e.cardBackButton()).
		Build()
}

func (e *Engine) renderProviderCard() *Card {
	switcher, ok := e.agent.(ProviderSwitcher)
	if !ok {
		return e.simpleCard(e.i18n.T(MsgCardTitleProvider), "indigo", e.i18n.T(MsgProviderNotSupported))
	}

	current := switcher.GetActiveProvider()
	providers := switcher.ListProviders()

	if current == nil && len(providers) == 0 {
		return e.simpleCard(e.i18n.T(MsgCardTitleProvider), "indigo", e.i18n.T(MsgProviderNone))
	}

	var body strings.Builder
	if current != nil {
		body.WriteString(fmt.Sprintf(e.i18n.T(MsgProviderCurrent), current.Name))
		body.WriteString("\n\n")
	}

	cb := NewCard().Title(e.i18n.T(MsgCardTitleProvider), "indigo").Markdown(body.String())
	if len(providers) > 0 {
		var opts []CardSelectOption
		initVal := ""
		for _, prov := range providers {
			label := prov.Name
			if prov.BaseURL != "" {
				label += " (" + prov.BaseURL + ")"
			}
			val := "act:/provider " + prov.Name
			opts = append(opts, CardSelectOption{Text: label, Value: val})
			if current != nil && prov.Name == current.Name {
				initVal = val
			}
		}
		cb.Select(e.i18n.T(MsgProviderSelectPlaceholder), opts, initVal)
	}
	return cb.Buttons(e.cardBackButton()).Build()
}

func (e *Engine) renderCronCard(sessionKey string) *Card {
	if e.cronScheduler == nil {
		return e.simpleCard(e.i18n.T(MsgCardTitleCron), "orange", e.i18n.T(MsgCronNotAvailable))
	}

	jobs := e.cronScheduler.Store().ListBySessionKey(sessionKey)
	if len(jobs) == 0 {
		return e.simpleCard(e.i18n.T(MsgCardTitleCron), "orange", e.i18n.T(MsgCronEmpty))
	}

	lang := e.i18n.CurrentLang()
	now := time.Now()
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(e.i18n.T(MsgCronListTitle), len(jobs)))
	sb.WriteString("\n\n")

	for i, j := range jobs {
		if i > 0 {
			sb.WriteString("\n")
		}
		status := "✅"
		if !j.Enabled {
			status = "⏸"
		}
		desc := j.Description
		if desc == "" {
			desc = truncateStr(j.Prompt, 60)
		}
		sb.WriteString(fmt.Sprintf("%s %s\n", status, desc))
		sb.WriteString(e.i18n.Tf(MsgCronIDLabel, j.ID))
		human := CronExprToHuman(j.CronExpr, lang)
		sb.WriteString(e.i18n.Tf(MsgCronScheduleLabel, human, j.CronExpr))
		nextRun := e.cronScheduler.NextRun(j.ID)
		if !nextRun.IsZero() {
			fmtStr := cronTimeFormat(nextRun, now)
			sb.WriteString(e.i18n.Tf(MsgCronNextRunLabel, nextRun.Format(fmtStr)))
		}
		if !j.LastRun.IsZero() {
			fmtStr := cronTimeFormat(j.LastRun, now)
			sb.WriteString(e.i18n.Tf(MsgCronLastRunLabel, j.LastRun.Format(fmtStr)))
			if j.LastError != "" {
				sb.WriteString(e.i18n.Tf(MsgCronFailedSuffix, truncateStr(j.LastError, 40)))
			}
			sb.WriteString("\n")
		}
	}

	return NewCard().Title(e.i18n.T(MsgCardTitleCron), "orange").
		Markdown(sb.String()).
		Buttons(e.cardBackButton()).
		Build()
}

func (e *Engine) renderCommandsCard() *Card {
	cmds := e.commands.ListAll()
	if len(cmds) == 0 {
		return e.simpleCard(e.i18n.T(MsgCardTitleCommands), "purple", e.i18n.T(MsgCommandsEmpty))
	}

	var sb strings.Builder
	sb.WriteString(e.i18n.Tf(MsgCommandsTitle, len(cmds)))
	for _, c := range cmds {
		tag := ""
		if c.Source == "agent" {
			tag = e.i18n.T(MsgCommandsTagAgent)
		} else if c.Exec != "" {
			tag = e.i18n.T(MsgCommandsTagShell)
		}
		desc := c.Description
		if desc == "" {
			if c.Exec != "" {
				desc = "$ " + truncateStr(c.Exec, 60)
			} else {
				desc = truncateStr(c.Prompt, 60)
			}
		}
		sb.WriteString(fmt.Sprintf("/%s%s — %s\n", c.Name, tag, desc))
	}

	return NewCard().Title(e.i18n.T(MsgCardTitleCommands), "purple").
		Markdown(sb.String()).
		Note(e.i18n.T(MsgCommandsHint)).
		Buttons(e.cardBackButton()).
		Build()
}

func (e *Engine) renderAliasCard() *Card {
	e.aliasMu.RLock()
	defer e.aliasMu.RUnlock()

	if len(e.aliases) == 0 {
		return e.simpleCard(e.i18n.T(MsgCardTitleAlias), "purple", e.i18n.T(MsgAliasEmpty))
	}

	names := make([]string, 0, len(e.aliases))
	for n := range e.aliases {
		names = append(names, n)
	}
	sort.Strings(names)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(e.i18n.T(MsgAliasListHeader), len(e.aliases)))
	sb.WriteString("\n")
	for _, n := range names {
		sb.WriteString(fmt.Sprintf("`%s` → `%s`\n", n, e.aliases[n]))
	}

	return NewCard().Title(e.i18n.T(MsgCardTitleAlias), "purple").
		Markdown(sb.String()).
		Buttons(e.cardBackButton()).
		Build()
}

func (e *Engine) renderConfigCard() *Card {
	items := e.configItems()
	isZh := e.i18n.IsZhLike()

	var sb strings.Builder
	sb.WriteString(e.i18n.T(MsgConfigTitle))
	for _, item := range items {
		sb.WriteString(fmt.Sprintf("`%s` = `%s`\n  %s\n\n", item.key, item.getFunc(), item.description(isZh)))
	}

	return NewCard().Title(e.i18n.T(MsgCardTitleConfig), "grey").
		Markdown(sb.String()).
		Note(e.i18n.T(MsgConfigHint)).
		Buttons(e.cardBackButton()).
		Build()
}

func (e *Engine) renderSkillsCard() *Card {
	skills := e.skills.ListAll()
	if len(skills) == 0 {
		return e.simpleCard(e.i18n.T(MsgCardTitleSkills), "purple", e.i18n.T(MsgSkillsEmpty))
	}

	var sb strings.Builder
	sb.WriteString(e.i18n.Tf(MsgSkillsTitle, e.agent.Name(), len(skills)))
	for _, s := range skills {
		sb.WriteString(fmt.Sprintf("  /%s — %s\n", s.Name, s.Description))
	}

	return NewCard().Title(e.i18n.T(MsgCardTitleSkills), "purple").
		Markdown(sb.String()).
		Note(e.i18n.T(MsgSkillsHint)).
		Buttons(e.cardBackButton()).
		Build()
}

func (e *Engine) renderDoctorCard() *Card {
	type doctorResult struct {
		results []DoctorCheckResult
	}
	ch := make(chan doctorResult, 1)
	go func() {
		ch <- doctorResult{RunDoctorChecks(e.ctx, e.agent, e.platforms)}
	}()

	var report string
	select {
	case res := <-ch:
		report = FormatDoctorResults(res.results, e.i18n)
	case <-time.After(2 * time.Second):
		report = "⏱ " + e.i18n.T(MsgDoctorTimeout)
	}

	return NewCard().
		Title(e.i18n.T(MsgCardTitleDoctor), "orange").
		Markdown(report).
		Buttons(e.cardBackButton()).
		Build()
}

func (e *Engine) renderVersionCard() *Card {
	return NewCard().
		Title(e.i18n.T(MsgCardTitleVersion), "grey").
		Markdown(VersionInfo).
		Buttons(e.cardBackButton()).
		Build()
}

func (e *Engine) renderUpgradeCard() *Card {
	title := e.i18n.T(MsgCardTitleUpgrade)
	cur := CurrentVersion
	if cur == "" || cur == "dev" {
		return e.simpleCard(title, "grey", e.i18n.T(MsgUpgradeDevBuild))
	}

	type result struct {
		release *ReleaseInfo
		err     error
	}
	ch := make(chan result, 1)
	useGitee := e.i18n.IsZhLike()
	go func() {
		r, err := CheckForUpdate(cur, useGitee)
		ch <- result{r, err}
	}()

	var content string
	select {
	case res := <-ch:
		if res.err != nil {
			content = fmt.Sprintf("❌ %s", res.err)
		} else if res.release == nil {
			content = fmt.Sprintf(e.i18n.T(MsgUpgradeUpToDate), cur)
		} else {
			body := res.release.Body
			if len([]rune(body)) > 300 {
				body = string([]rune(body)[:300]) + "…"
			}
			content = fmt.Sprintf(e.i18n.T(MsgUpgradeAvailable), cur, res.release.TagName, body)
		}
	case <-time.After(2 * time.Second):
		content = "⏱ " + e.i18n.T(MsgUpgradeChecking) + e.i18n.T(MsgUpgradeTimeoutSuffix)
	}

	return NewCard().
		Title(title, "grey").
		Markdown(content).
		Buttons(e.cardBackButton()).
		Build()
}

// ──────────────────────────────────────────────────────────────
