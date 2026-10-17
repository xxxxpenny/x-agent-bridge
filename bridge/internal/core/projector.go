package core

import (
	"context"
	"strings"
)

// StartAdapterRun 启动一轮适配器执行（投影器 goroutine）。
func StartAdapterRun(ctx context.Context, task *Task, prompt string) {
	// thread → runtime 原生会话句柄：由 adapter 自管（SessionBinder）
	var prev string
	var save func(string)
	if b, ok := task.adapter.(SessionBinder); ok {
		prev = b.SessionHandle(task.ContextID)
		save = func(id string) { b.SaveSessionHandle(task.ContextID, id) }
	}

	go func() {
		var sb, rsb strings.Builder
		err := task.adapter.Run(ctx, AdapterRequest{
			TaskID: task.TaskID, ContextID: task.ContextID, Prompt: prompt, InputCh: task.inputCh,
			PrevSession: prev, SaveSessionID: save,
		}, func(ev RunEvent) {
			project(task, ev, &sb, &rsb)
		})
		task.mu.Lock()
		st := task.Status
		task.mu.Unlock()
		if !st.Terminal() {
			if err != nil {
				project(task, RunEvent{Kind: "done", Err: err}, &sb, &rsb)
			} else {
				project(task, RunEvent{Kind: "done"}, &sb, &rsb)
			}
		}
	}()
}

func project(task *Task, ev RunEvent, sb, rsb *strings.Builder) {
	appendHist := func(role, content, thinking string) {
		if h, ok := task.adapter.(SessionHistory); ok {
			h.AppendMessage(task.ContextID, HistoryMsg{
				Role: role, Content: content, Thinking: thinking,
				TaskID: task.TaskID, CreatedAt: NowRFC3339(),
			})
		}
	}
	switch ev.Kind {
	case "status":
		task.setStatus(ev.Status, ev.Message)

	case "text":
		sb.WriteString(ev.Text)
		forwardRunEvent(task, ev)

	case "reasoning_start":
		if rsb.Len() > 0 {
			rsb.WriteString("\n\n")
		}
		forwardRunEvent(task, ev)

	case "reasoning", "reasoning_end":
		if ev.Kind == "reasoning" {
			rsb.WriteString(ev.Text)
		}
		forwardRunEvent(task, ev)

	case "input":
		// HITL 暂停：已流出内容先落库并重置，续跑作为下一条 assistant 消息
		if sb.Len() > 0 || rsb.Len() > 0 {
			appendHist("assistant", sb.String(), rsb.String())
			sb.Reset()
			rsb.Reset()
		}
		task.mu.Lock()
		task.RequiredInput = ev.Input
		task.UpdatedAt = NowRFC3339()
		task.mu.Unlock()
		task.setStatus(StateInputRequired, ev.Input.Prompt)

	case "toolcall", "pause":
		forwardRunEvent(task, ev)

	case "done":
		task.mu.Lock()
		already := task.Status.Terminal()
		task.mu.Unlock()
		if !already {
			if ev.Err != nil {
				task.mu.Lock()
				task.Error = map[string]any{"code": "runtime_error", "message": ev.Err.Error()}
				task.mu.Unlock()
				task.setStatus(StateFailed, ev.Err.Error())
			} else {
				task.setStatus(StateCompleted, "completed")
			}
		}
		text := sb.String()
		if text != "" || rsb.Len() > 0 {
			appendHist("assistant", text, rsb.String())
		}
		forwardRunEvent(task, ev)
	}
}

// forwardRunEvent 投递给已挂载的 AG-UI run；大缓冲避免 token 级 delta 丢字。
func forwardRunEvent(task *Task, ev RunEvent) {
	task.runSubMu.Lock()
	sub := task.runSub
	task.runSubMu.Unlock()
	if sub == nil {
		return
	}
	select {
	case sub <- ev:
	default:
	}
}

// AttachRun / DetachRun 管理 AG-UI run 订阅。
func AttachRun(t *Task) chan RunEvent {
	t.runSubMu.Lock()
	defer t.runSubMu.Unlock()
	ch := make(chan RunEvent, 8192)
	t.runSub = ch
	return ch
}

func DetachRun(t *Task) {
	t.runSubMu.Lock()
	t.runSub = nil
	t.runSubMu.Unlock()
}

// ResolveInput 校验并投递 HITL 冲突返回 false。
func ResolveInput(t *Task, resp InputResponse) bool {
	t.mu.Lock()
	if t.Status != StateInputRequired || t.RequiredInput == nil || t.RequiredInput.InputID != resp.InputID {
		t.mu.Unlock()
		return false
	}
	t.RequiredInput = nil
	t.mu.Unlock()
	select {
	case t.inputCh <- resp:
		return true
	default:
		return false
	}
}
