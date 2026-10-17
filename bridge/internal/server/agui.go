package server

// AG-UI 聊天面：POST /api/v1/{rt}/ag-ui —— 事件线格式（@ag-ui/encoder 兼容：
// data: {json}\n\n），HITL 经 bridge_hitl toolCall 折叠续跑。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"xagent-bridge/internal/core"
)

const (
	evRunStarted        = "RUN_STARTED"
	evRunFinished       = "RUN_FINISHED"
	evRunError          = "RUN_ERROR"
	evStateSnapshot     = "STATE_SNAPSHOT"
	evTextMessageStart  = "TEXT_MESSAGE_START"
	evTextMessageContent = "TEXT_MESSAGE_CONTENT"
	evTextMessageEnd    = "TEXT_MESSAGE_END"
	evToolCallStart     = "TOOL_CALL_START"
	evToolCallArgs      = "TOOL_CALL_ARGS"
	evToolCallEnd       = "TOOL_CALL_END"
)

type AguiEvent struct {
	Type            string          `json:"type"`
	ThreadID        string          `json:"threadId,omitempty"`
	RunID           string          `json:"runId,omitempty"`
	MessageID       string          `json:"messageId,omitempty"`
	Role            string          `json:"role,omitempty"`
	Delta           string          `json:"delta,omitempty"`
	ToolCallID      string          `json:"toolCallId,omitempty"`
	ToolCallName    string          `json:"toolCallName,omitempty"`
	ParentMessageID string          `json:"parentMessageId,omitempty"`
	Snapshot        json.RawMessage `json:"snapshot,omitempty"`
	Message         string          `json:"message,omitempty"`
}

type AguiMessage struct {
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ToolCalls []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"toolCalls"`
	ToolCallID string `json:"toolCallId"`
}

func (m AguiMessage) TextContent() string {
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		out := ""
		for _, p := range parts {
			if p.Type == "text" {
				out += p.Text
			}
		}
		return out
	}
	return ""
}

type RunAgentInput struct {
	ThreadID    string            `json:"threadId"`
	RunID       string            `json:"runId"`
	Messages    []AguiMessage     `json:"messages"`
	State       json.RawMessage   `json:"state"`
	Resume      []json.RawMessage `json:"resume"`
	Attachments []core.Attachment `json:"attachments"` // 本轮上传的附件元数据（path 为 bridge 落盘绝对路径）
}

func (in *RunAgentInput) FindHITLResult() (toolCallID, payload string, ok bool) {
	for i := len(in.Messages) - 1; i >= 0; i-- {
		m := in.Messages[i]
		if m.Role == "tool" && m.ToolCallID != "" {
			return m.ToolCallID, m.TextContent(), true
		}
	}
	return "", "", false
}

func (in *RunAgentInput) LastUserText() string {
	for i := len(in.Messages) - 1; i >= 0; i-- {
		if in.Messages[i].Role == "user" {
			return in.Messages[i].TextContent()
		}
	}
	return ""
}

type sseWriter struct {
	w interface {
		Write([]byte) (int, error)
	}
	f func()
}

func (s *sseWriter) send(ev AguiEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return err
	}
	if s.f != nil {
		s.f()
	}
	return nil
}

// handleAGUI implements POST /api/v1/{rt}/ag-ui.
func (s *Server) handleAGUI(w http.ResponseWriter, r *http.Request, runtimeID string) {
	rt, ok := s.adapters[runtimeID]
	if !ok {
		writeErr(w, http.StatusNotFound, "runtime_not_found", "unknown runtime "+runtimeID, nil)
		return
	}
	if !rt.Adapter.Capabilities()["agui"] {
		writeErr(w, http.StatusNotImplemented, "capability_not_supported", "runtime has no agui", nil)
		return
	}

	var in RunAgentInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "bad RunAgentInput: "+err.Error(), nil)
		return
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	enc := &sseWriter{w: w, f: func() {
		if flusher != nil {
			flusher.Flush()
		}
	}}

	threadID := in.ThreadID
	if threadID == "" {
		threadID = core.NewID("ctx_")
	}
	runID := in.RunID
	if runID == "" {
		runID = core.NewID("run_")
	}

	// --- HITL continuation: run carries a bridge_hitl tool result ---
	if tcID, payload, ok := in.FindHITLResult(); ok {
		task := s.findPendingTask(runtimeID, threadID, tcID)
		if task == nil {
			enc.send(AguiEvent{Type: "RUN_ERROR", ThreadID: threadID, RunID: runID, Message: "no pending input for tool call " + tcID})
			return
		}
		summary := ""
		if in2 := task.PendingInput(); in2 != nil {
			summary = hitlSummary(in2.Prompt)
		}
		resp := core.InputResponse{InputID: tcID, Action: parseHITLAction(payload)}
		// attach 必须先于 resolve：adapter 放行后会立刻发后续事件
		sub := core.AttachRun(task)
		defer core.DetachRun(task)
		if !core.ResolveInput(task, resp) {
			enc.send(AguiEvent{Type: "RUN_ERROR", ThreadID: threadID, RunID: runID, Message: "input_not_pending"})
			return
		}
		if h, ok := task.BoundAdapter().(core.SessionHistory); ok {
			h.AppendMessage(task.ContextID, core.HistoryMsg{
				Role: "tool", TaskID: task.TaskID, CreatedAt: core.NowRFC3339(),
				Content: core.MustJSON(map[string]any{"action": resp.Action, "summary": summary}),
			})
		}
		s.loopRun(r.Context(), enc, task, runID, threadID, sub)
		return
	}

	// --- new task ---
	userText := in.LastUserText()
	if strings.TrimSpace(userText) == "" {
		enc.send(AguiEvent{Type: "RUN_ERROR", ThreadID: threadID, RunID: runID, Message: "empty user message"})
		return
	}
	task := s.startTask(runtimeID, threadID, userText, rt.Adapter, in.Attachments...)
	sub := core.AttachRun(task)
	defer core.DetachRun(task)
	s.loopRun(r.Context(), enc, task, runID, threadID, sub)
}

// loopRun 将已挂载的订阅翻译为 AG-UI SSE。
func (s *Server) loopRun(reqCtx context.Context, enc *sseWriter, task *core.Task, runID, threadID string, sub chan core.RunEvent) {
	enc.send(AguiEvent{Type: evRunStarted, ThreadID: threadID, RunID: runID})
	enc.send(AguiEvent{Type: evStateSnapshot, ThreadID: threadID, RunID: runID, Snapshot: task.SnapshotJSON()})

	textOpen := false
	msgID := core.NewID("msg_")
	reasoningOpen := false
	rsnID := core.NewID("rsn_")
	closeText := func() {
		if textOpen {
			enc.send(AguiEvent{Type: evTextMessageEnd, ThreadID: threadID, RunID: runID, MessageID: msgID})
			textOpen = false
		}
	}
	closeReasoning := func() {
		if reasoningOpen {
			enc.send(AguiEvent{Type: "REASONING_MESSAGE_END", ThreadID: threadID, RunID: runID, MessageID: rsnID})
			reasoningOpen = false
		}
	}
	finishOK := func() {
		closeText()
		closeReasoning()
		enc.send(AguiEvent{Type: evStateSnapshot, ThreadID: threadID, RunID: runID, Snapshot: task.SnapshotJSON()})
		enc.send(AguiEvent{Type: evRunFinished, ThreadID: threadID, RunID: runID})
	}

	for {
		select {
		case <-reqCtx.Done():
			return // client went away; task keeps running in background
		case ev := <-sub:
			switch ev.Kind {
			case "status":
				enc.send(AguiEvent{Type: evStateSnapshot, ThreadID: threadID, RunID: runID, Snapshot: task.SnapshotJSON()})

			case "text":
				closeReasoning()
				if !textOpen {
					enc.send(AguiEvent{Type: evTextMessageStart, ThreadID: threadID, RunID: runID, MessageID: msgID, Role: "assistant"})
					textOpen = true
				}
				enc.send(AguiEvent{Type: evTextMessageContent, ThreadID: threadID, RunID: runID, MessageID: msgID, Delta: ev.Text})

			case "reasoning_start":
				rsnID = core.NewID("rsn_")
				reasoningOpen = true
				enc.send(AguiEvent{Type: "REASONING_MESSAGE_START", ThreadID: threadID, RunID: runID, MessageID: rsnID, Role: "reasoning"})

			case "reasoning":
				if !reasoningOpen {
					rsnID = core.NewID("rsn_")
					reasoningOpen = true
					enc.send(AguiEvent{Type: "REASONING_MESSAGE_START", ThreadID: threadID, RunID: runID, MessageID: rsnID, Role: "reasoning"})
				}
				enc.send(AguiEvent{Type: "REASONING_MESSAGE_CONTENT", ThreadID: threadID, RunID: runID, MessageID: rsnID, Delta: ev.Text})

			case "reasoning_end":
				closeReasoning()

			case "toolcall":
				closeText()
				closeReasoning()
				enc.send(AguiEvent{Type: evToolCallStart, ThreadID: threadID, RunID: runID, ToolCallID: ev.ToolCallID, ToolCallName: ev.ToolCallName, ParentMessageID: msgID})
				enc.send(AguiEvent{Type: evToolCallArgs, ThreadID: threadID, RunID: runID, ToolCallID: ev.ToolCallID, Delta: ev.ToolArgs})
				enc.send(AguiEvent{Type: evToolCallEnd, ThreadID: threadID, RunID: runID, ToolCallID: ev.ToolCallID})

			case "pause":
				finishOK()
				return

			case "done":
				failed, errMsg := task.Failure()
				if failed {
					closeText()
					closeReasoning()
					enc.send(AguiEvent{Type: evStateSnapshot, ThreadID: threadID, RunID: runID, Snapshot: task.SnapshotJSON()})
					enc.send(AguiEvent{Type: "RUN_ERROR", ThreadID: threadID, RunID: runID, Message: errMsg})
				} else {
					finishOK()
				}
				return
			}
		}
	}
}

func (s *Server) findPendingTask(runtimeID, threadID, inputID string) *core.Task {
	for _, t := range s.store.ListTasks(runtimeID, threadID, []string{string(core.StateInputRequired)}) {
		if in := t.PendingInput(); in != nil && in.InputID == inputID {
			return t
		}
	}
	return nil
}

// hitlSummary: "允许执行工具 Bash 吗？\n入参: {...}" → "Bash：touch x"
func hitlSummary(prompt string) string {
	lines := strings.SplitN(prompt, "\n", 2)
	first := lines[0]
	tool := ""
	if m := strings.Index(first, "工具 "); m >= 0 {
		rest := first[m+len("工具 "):]
		if i := strings.Index(rest, " "); i > 0 {
			tool = rest[:i]
		}
	}
	if len(lines) < 2 || !strings.HasPrefix(strings.TrimSpace(lines[1]), "入参:") {
		return first
	}
	inputPart := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[1]), "入参:"))
	var input map[string]any
	if err := json.Unmarshal([]byte(inputPart), &input); err == nil {
		if cmd, ok := input["command"].(string); ok && cmd != "" {
			if tool != "" {
				return tool + "：" + cmd
			}
			return cmd
		}
	}
	if tool != "" {
		return tool
	}
	return first
}

func parseHITLAction(payload string) string {
	p := strings.TrimSpace(payload)
	if p == "" {
		return "approve"
	}
	lower := strings.ToLower(p)
	if strings.Contains(lower, "reject") || strings.Contains(lower, "deny") ||
		strings.Contains(lower, "\"approved\":false") || strings.Contains(lower, "\"approved\": false") {
		return "reject"
	}
	return "approve"
}
