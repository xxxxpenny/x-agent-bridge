// Package memory: 脚本演示适配器（test/测试 触发预检式 HITL）。
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"xagent-bridge/internal/core"
)

type Adapter struct {
	mu       sync.Mutex
	sessions map[string]*memSession
}

type memSession struct {
	Title     string
	TaskCount int
	UpdatedAt string
	Messages  []core.HistoryMsg
}

func NewAdapter() *Adapter {
	return &Adapter{sessions: map[string]*memSession{}}
}

// ---- core.SessionStore / SessionHistory（内存实现） ----

func (m *Adapter) EnsureSession(id, seedTitle string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		s = &memSession{}
		m.sessions[id] = s
	}
	if s.Title == "" && seedTitle != "" {
		s.Title = seedTitle
	}
	s.TaskCount++
	s.UpdatedAt = core.NowRFC3339()
	return nil
}

func (m *Adapter) ListSessions() ([]core.SessionMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []core.SessionMeta{}
	for id, s := range m.sessions {
		title := s.Title
		if title == "" {
			title = "New session"
		}
		out = append(out, core.SessionMeta{ID: id, Title: title, TaskCount: s.TaskCount, UpdatedAt: s.UpdatedAt})
	}
	return out, nil
}

func (m *Adapter) SetTitle(id, title string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		s.Title = title
		s.UpdatedAt = core.NowRFC3339()
	}
	return nil
}

func (m *Adapter) ListMessages(id string) ([]core.HistoryMsg, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, nil
	}
	out := make([]core.HistoryMsg, len(s.Messages))
	copy(out, s.Messages)
	return out, nil
}

func (m *Adapter) AppendMessage(id string, msg core.HistoryMsg) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		s = &memSession{}
		m.sessions[id] = s
	}
	s.Messages = append(s.Messages, msg)
	s.UpdatedAt = core.NowRFC3339()
	return nil
}

func (m *Adapter) ID() string   { return "memory" }
func (m *Adapter) Name() string { return "Memory Adapter (spike)" }
func (m *Adapter) Health() (string, string) {
	return "online", "0.1.0"
}
func (m *Adapter) Capabilities() map[string]bool {
	return map[string]bool{
		"agui": true, "streaming": true, "hitl": true, "sessions": true,
		"skills": false, "mcp": false, "files": false,
	}
}

func (m *Adapter) Cancel() {}

func (m *Adapter) Run(ctx context.Context, req core.AdapterRequest, sink func(core.RunEvent)) error {
	sink(core.RunEvent{Kind: "status", Status: core.StateWorking, Message: "正在思考..."})

	prompt := req.Prompt
	lower := strings.ToLower(prompt)
	needsHITL := strings.Contains(lower, "test") || strings.Contains(prompt, "测试") || strings.Contains(lower, "npm")

	intro := fmt.Sprintf("收到任务：%s\n", prompt)
	if err := core.StreamText(ctx, sink, intro, 8, 30*time.Millisecond); err != nil {
		return err
	}

	if needsHITL {
		inputID := core.NewID("inp_")
		input := &core.RequiredInput{
			InputID: inputID,
			Type:    "approval",
			Prompt:  "是否允许执行 Shell 命令: npm test?",
		}
		sink(core.RunEvent{Kind: "input", Input: input})

		args, _ := json.Marshal(input)
		// toolCallId == input_id（§5.3）
		sink(core.RunEvent{Kind: "toolcall", ToolCallID: inputID, ToolCallName: "bridge_hitl", ToolArgs: string(args)})
		sink(core.RunEvent{Kind: "pause"})

		var resp core.InputResponse
		select {
		case resp = <-req.InputCh:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Minute):
			return fmt.Errorf("hitl_timeout")
		}

		if resp.Action == "reject" {
			sink(core.RunEvent{Kind: "status", Status: core.StateWorking, Message: "已拒绝执行"})
			sink(core.RunEvent{Kind: "text", Text: "好的，不执行 npm test。如需运行请再次发起。"})
			sink(core.RunEvent{Kind: "done"})
			return nil
		}

		sink(core.RunEvent{Kind: "status", Status: core.StateWorking, Message: "已批准，正在执行 npm test..."})
		sink(core.RunEvent{Kind: "text", Text: "（模拟）$ npm test\n✓ all 3 tests passed (0.4s)\n"})
		sink(core.RunEvent{Kind: "done"})
		return nil
	}

	reply := "这是一个 MemoryAdapter 的演示回复。" + prompt + " 已记录。如需触发 HITL，请让我「运行测试」或包含 test 关键字。"
	if err := core.StreamText(ctx, sink, reply, 10, 25*time.Millisecond); err != nil {
		return err
	}
	sink(core.RunEvent{Kind: "done"})
	return nil
}
