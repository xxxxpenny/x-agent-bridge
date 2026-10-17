package core

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// ---- Task / Session 模型（A2A 对齐语义） ----

type Task struct {
	mu            sync.Mutex
	TaskID        string         `json:"task_id"`
	ContextID     string         `json:"context_id"`
	RuntimeID     string         `json:"runtime_id"`
	Status        TaskState      `json:"status"`
	StatusMessage string         `json:"status_message"`
	Input         map[string]any `json:"input"`
	RequiredInput *RequiredInput `json:"required_input"`
	Artifacts     []Artifact     `json:"artifacts"`
	Error         any            `json:"error"`
	CreatedAt     string         `json:"created_at"`
	UpdatedAt     string         `json:"updated_at"`
	EndedAt       string         `json:"ended_at"`

	// runtime coordination (not serialized)
	adapter    Adapter
	inputCh    chan InputResponse
	runCtx     context.Context
	runCancel  context.CancelFunc
	runSubMu   sync.Mutex
	runSub     chan RunEvent // attached AG-UI run subscriber, nil when none
	promptText string
}

// ---- Task 上下文安全访问（供 server 包使用） ----

func (t *Task) SnapshotJSON() json.RawMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, _ := json.Marshal(t)
	return b
}

func (t *Task) View() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, _ := json.Marshal(t)
	var v map[string]any
	json.Unmarshal(b, &v)
	return v
}

func (t *Task) IsTerminal() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Status.Terminal()
}

func (t *Task) PendingInput() *RequiredInput {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.RequiredInput
}

func (t *Task) SetStatus(st TaskState, msg string) {
	t.setStatus(st, msg)
}

// Failure 返回任务是否失败及错误消息。
func (t *Task) Failure() (failed bool, msg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Status != StateFailed {
		return false, ""
	}
	if e, ok := t.Error.(map[string]any); ok {
		msg, _ = e["message"].(string)
	}
	return true, msg
}

func (t *Task) CancelRun() {
	if t.runCancel != nil {
		t.runCancel()
	}
}

func (t *Task) setStatus(st TaskState, msg string) {
	t.mu.Lock()
	t.Status = st
	if msg != "" {
		t.StatusMessage = msg
	}
	t.UpdatedAt = NowRFC3339()
	if st.Terminal() {
		t.EndedAt = NowRFC3339()
	}
	t.mu.Unlock()
	t.snapshotToRun()
}

// snapshotToRun pushes the latest status to the attached AG-UI run, if any.
func (t *Task) snapshotToRun() {
	t.runSubMu.Lock()
	sub := t.runSub
	t.runSubMu.Unlock()
	if sub == nil {
		return
	}
	select {
	case sub <- RunEvent{Kind: "status", Status: t.Status, Message: t.StatusMessage}:
	default:
	}
}

func NowRFC3339() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }

// View 返回会话的对外快照（线程安全）。
// ---- Store ----

// Persister 是持久化后端（internal/persist 实现）；nil = 仅内存。

// ---- Store（仅任务运行时记录；会话/历史由 adapter 自管） ----

type Store struct {
	mu    sync.RWMutex
	tasks map[string]*Task
	order []string // task ids, newest last
}

func NewStore() *Store {
	return &Store{tasks: map[string]*Task{}}
}

func (s *Store) GetTask(id string) *Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tasks[id]
}

func (s *Store) PutTask(t *Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[t.TaskID] = t
	s.order = append(s.order, t.TaskID)
}

func (s *Store) ListTasks(runtimeID, contextID string, states []string) []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*Task{}
	stateSet := map[string]bool{}
	for _, st := range states {
		stateSet[st] = true
	}
	for _, id := range s.order {
		t := s.tasks[id]
		if runtimeID != "" && t.RuntimeID != runtimeID {
			continue
		}
		if contextID != "" && t.ContextID != contextID {
			continue
		}
		if len(stateSet) > 0 && !stateSet[string(t.Status)] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// NewTask 注册任务并把 user 消息/会话种子写入 adapter 侧历史；调用方负责 StartAdapterRun。
// atts 为该轮上传的附件元数据（随 user 消息持久化，供 UI 回显）。
func (s *Store) NewTask(runtimeID, threadID, prompt string, ad Adapter, atts ...Attachment) *Task {
	ctx, cancel := context.WithCancel(context.Background())
	t := &Task{
		TaskID:     NewID("task_"),
		ContextID:  threadID,
		RuntimeID:  runtimeID,
		Status:     StateSubmitted,
		Input:      map[string]any{"message": prompt},
		CreatedAt:  NowRFC3339(),
		UpdatedAt:  NowRFC3339(),
		adapter:    ad,
		inputCh:    make(chan InputResponse, 4),
		runCtx:     ctx,
		runCancel:  cancel,
		promptText: prompt,
	}
	s.PutTask(t)

	seed := prompt
	if runes := []rune(seed); len(runes) > 36 {
		seed = string(runes[:36]) + "…"
	}
	if ss, ok := ad.(SessionStore); ok {
		_ = ss.EnsureSession(threadID, seed)
	}
	if h, ok := ad.(SessionHistory); ok {
		h.AppendMessage(threadID, HistoryMsg{
			Role: "user", Content: prompt, TaskID: t.TaskID, CreatedAt: NowRFC3339(),
			Attachments: atts,
		})
	}
	return t
}

// TaskRunContext 返回任务的后台 ctx（投影器用）。
func (t *Task) TaskRunContext() context.Context { return t.runCtx }

// BoundAdapter 返回任务绑定的适配器。
func (t *Task) BoundAdapter() Adapter { return t.adapter }
