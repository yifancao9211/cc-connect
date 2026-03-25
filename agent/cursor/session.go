package cursor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

// cursorSession manages a multi-turn Cursor Agent conversation.
// Each Send() spawns `agent --print --output-format stream-json <prompt>`.
// Subsequent turns use `--resume-session <sessionID>` to resume.
type cursorSession struct {
	cmd       string
	workDir   string
	model     string
	mode      string
	extraEnv  []string
	events    chan core.Event
	sessionID atomic.Value // stores string
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	alive     atomic.Bool
}

func newCursorSession(ctx context.Context, cmd, workDir, model, mode, resumeID string, extraEnv []string) (*cursorSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)

	cs := &cursorSession{
		cmd:      cmd,
		workDir:  workDir,
		model:    model,
		mode:     mode,
		extraEnv: extraEnv,
		events:   make(chan core.Event, 64),
		ctx:      sessionCtx,
		cancel:   cancel,
	}
	cs.alive.Store(true)

	if resumeID != "" {
		cs.sessionID.Store(resumeID)
	}

	return cs, nil
}

func (cs *cursorSession) Send(prompt string, images []core.ImageAttachment, files []core.FileAttachment) error {
	if len(images) > 0 {
		slog.Warn("cursorSession: images not supported, ignoring")
	}
	if len(files) > 0 {
		filePaths := core.SaveFilesToDisk(cs.workDir, files)
		prompt = core.AppendFileRefs(prompt, filePaths)
	}
	if !cs.alive.Load() {
		return fmt.Errorf("session is closed")
	}

	args := []string{"--print", "--output-format", "stream-json"}

	sid := cs.CurrentSessionID()
	if sid != "" {
		args = append(args, "--resume-session", sid)
	}

	switch cs.mode {
	case "force":
		args = append(args, "--force")
	case "plan":
		args = append(args, "--plan")
	case "ask":
		args = append(args, "--ask")
	}

	if cs.model != "" {
		args = append(args, "--model", cs.model)
	}

	args = append(args, prompt)

	slog.Debug("cursorSession: launching", "resume", sid != "", "args_len", len(args))

	cmd := exec.CommandContext(cs.ctx, cs.cmd, args...)
	cmd.Dir = cs.workDir
	if len(cs.extraEnv) > 0 {
		cmd.Env = core.MergeEnv(os.Environ(), cs.extraEnv)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("cursorSession: stdout pipe: %w", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cursorSession: start: %w", err)
	}

	cs.wg.Add(1)
	go cs.readLoop(cmd, stdout, &stderrBuf)

	return nil
}

func (cs *cursorSession) readLoop(cmd *exec.Cmd, stdout io.ReadCloser, stderrBuf *bytes.Buffer) {
	defer cs.wg.Done()
	defer func() {
		if err := cmd.Wait(); err != nil {
			stderrMsg := strings.TrimSpace(stderrBuf.String())
			if stderrMsg != "" {
				slog.Error("cursorSession: process failed", "error", err, "stderr", truncStr(stderrMsg, 200))
				evt := core.Event{Type: core.EventError, Error: fmt.Errorf("%s", stderrMsg)}
				select {
				case cs.events <- evt:
				case <-cs.ctx.Done():
					return
				}
			}
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var raw streamEvent
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			slog.Debug("cursorSession: non-JSON line", "line", truncStr(line, 100))
			continue
		}

		cs.handleEvent(&raw)
	}

	if err := scanner.Err(); err != nil {
		slog.Error("cursorSession: scanner error", "error", err)
		evt := core.Event{Type: core.EventError, Error: fmt.Errorf("read stdout: %w", err)}
		select {
		case cs.events <- evt:
		case <-cs.ctx.Done():
			return
		}
	}
}

// ── stream-json event structures ─────────────────────────────

type streamEvent struct {
	Type      string         `json:"type"`
	Subtype   string         `json:"subtype"`
	SessionID string         `json:"session_id"`
	Done      bool           `json:"done"`
	Message   *streamMessage `json:"message"`
}

type streamMessage struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Status  string          `json:"status"`
	Content json.RawMessage `json:"content"`
}

type contentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Name     string `json:"name"`
	Input    string `json:"input"`
	Reason   string `json:"reason"`
	Content  string `json:"content"`
	Finished bool   `json:"finished"`
}

// ── event handling ───────────────────────────────────────────

func (cs *cursorSession) handleEvent(ev *streamEvent) {
	if ev.SessionID != "" {
		cs.sessionID.Store(ev.SessionID)
	}

	switch ev.Type {
	case "system":
		slog.Debug("cursorSession: init", "session_id", ev.SessionID)

	case "assistant":
		cs.handleAssistant(ev)

	case "result":
		cs.handleResult(ev)
	}
}

func (cs *cursorSession) handleAssistant(ev *streamEvent) {
	if ev.Message == nil {
		return
	}

	if ev.Message.Status != "finished" {
		return
	}

	var items []contentItem
	if err := json.Unmarshal(ev.Message.Content, &items); err != nil {
		return
	}

	for _, item := range items {
		switch item.Type {
		case "text":
			if item.Text != "" {
				evt := core.Event{Type: core.EventText, Content: item.Text}
				select {
				case cs.events <- evt:
				case <-cs.ctx.Done():
					return
				}
			}

		case "function":
			inputPreview := extractToolPreview(item.Input)
			evt := core.Event{Type: core.EventToolUse, ToolName: item.Name, ToolInput: inputPreview}
			select {
			case cs.events <- evt:
			case <-cs.ctx.Done():
				return
			}
		}
	}
}

func (cs *cursorSession) handleResult(ev *streamEvent) {
	var finalText string
	if ev.Message != nil {
		var items []contentItem
		if err := json.Unmarshal(ev.Message.Content, &items); err == nil {
			for _, item := range items {
				if item.Type == "text" && item.Text != "" {
					finalText = item.Text
				}
			}
		}
	}

	evt := core.Event{Type: core.EventResult, Content: finalText, SessionID: cs.CurrentSessionID(), Done: true}
	select {
	case cs.events <- evt:
	case <-cs.ctx.Done():
		return
	}
}

func (cs *cursorSession) RespondPermission(_ string, _ core.PermissionResult) error {
	return nil
}

func (cs *cursorSession) Events() <-chan core.Event {
	return cs.events
}

func (cs *cursorSession) CurrentSessionID() string {
	v, _ := cs.sessionID.Load().(string)
	return v
}

func (cs *cursorSession) Alive() bool {
	return cs.alive.Load()
}

func (cs *cursorSession) Close() error {
	cs.alive.Store(false)
	cs.cancel()
	done := make(chan struct{})
	go func() {
		cs.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		slog.Warn("cursorSession: close timed out, abandoning wg.Wait")
	}
	close(cs.events)
	return nil
}

// ── helpers ──────────────────────────────────────────────────

func extractToolPreview(inputJSON string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(inputJSON), &m); err != nil {
		return inputJSON
	}
	if cmd, ok := m["command"].(string); ok {
		return cmd
	}
	if file, ok := m["file_path"].(string); ok {
		return file
	}
	if pattern, ok := m["pattern"].(string); ok {
		return pattern
	}
	if query, ok := m["query"].(string); ok {
		return query
	}
	return inputJSON
}

func truncStr(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "..."
}
