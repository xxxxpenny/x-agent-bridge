package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"xagent-bridge/internal/core"
	"xagent-bridge/internal/runtimes/sesscache"
)

// ClaudeCodeAdapter drives the Claude Code CLI in headless mode:
//
//	claude -p <prompt> --output-format stream-json --verbose --dangerously-skip-permissions=false
//
// HITL: without the Agent SDK's canUseTool callback the CLI cannot pause
// mid-run over plain stream-json. For the spike the adapter asks for a
// pre-flight approval via bridge_hitl whenever the prompt looks like a
// shell/tool action; the actual permission wiring (MCP permission-prompt-tool)
// is documented in T3 notes for a follow-up.
type Adapter struct {
	Binary  string
	WorkDir string

	mu      sync.Mutex
	running map[string]*exec.Cmd
	*sesscache.Cache // 会话缓存：SessionStore/SessionHistory
}

// NewAdapter 构造适配器；claude CLI 缺失时返回 nil。
// workDir 是 claude 会话的工作目录（项目根）；dataDir 为会话缓存目录。
func NewAdapter(workDir, dataDir string) *Adapter {
	bin, err := exec.LookPath("claude")
	if err != nil {
		// also accept the npm global install location
		home, _ := os.UserHomeDir()
		cand := filepath.Join(home, ".npm-global/bin/claude")
		if _, err2 := os.Stat(cand); err2 == nil {
			bin = cand
		} else {
			return nil
		}
	}
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	return &Adapter{
		Binary: bin, WorkDir: workDir, running: map[string]*exec.Cmd{},
		Cache: sesscache.Open(dataDir, "claude-code-legacy"),
	}
}

// core.SessionBinder：thread → claude session_id。
func (c *Adapter) SessionHandle(thread string) string      { return c.Cache.Handle(thread) }
func (c *Adapter) SaveSessionHandle(thread, handle string) { c.Cache.SetHandle(thread, handle) }

func (c *Adapter) ID() string   { return "claude-code" }
func (c *Adapter) Name() string { return "Claude Code CLI" }
func (c *Adapter) Health() (string, string) {
	if c.Binary == "" {
		return "offline", ""
	}
	return "online", "cli"
}
func (c *Adapter) Capabilities() map[string]bool {
	return map[string]bool{
		"agui": true, "streaming": true, "hitl": true, "sessions": true,
		"skills": true, "mcp": true, "files": false,
	}
}

func (c *Adapter) Cancel() {}

type claudeStreamMsg struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	// control_request / control_response 的字段在顶层（对齐 SDK 读法）
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
	Response  json.RawMessage `json:"response"`
	Message   struct {
		Role    string `json:"role"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
	Result string `json:"result"`
	Error  struct {
		Message string `json:"message"`
	} `json:"error"`
	Event struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
		ContentBlock struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content_block"`
	} `json:"event"`
}

// controlRequest is the payload of a control_request's "request" field.
type controlRequest struct {
	Subtype  string          `json:"subtype"`
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
}

// Run follows the claude-agent-sdk-python transport (subprocess_cli.py):
// bidirectional stream-json over stdin/stdout, control-protocol handshake,
// can_use_tool permission callbacks, and --resume=<id> session continuation.
func (c *Adapter) Run(ctx context.Context, req core.AdapterRequest, sink func(core.RunEvent)) error {
	sink(core.RunEvent{Kind: "status", Status: core.StateWorking, Message: "launching claude code..."})

	args := []string{
		"--output-format", "stream-json",
		"--verbose",
		// token 级增量（stream_event/content_block_delta）
		"--include-partial-messages",
		// SDK 模式：prompt 走 stdin 协议，can_use_tool 才能回来
		"--input-format", "stream-json",
		// 关键：声明权限请求走 stdio 控制通道（SDK 的 canUseTool 即此机制）。
		// 缺了它，非交互模式下需要权限的工具会被直接 deny。
		"--permission-prompt-tool", "stdio",
		// manual 模式：危险工具触发 can_use_tool 控制请求（真·HITL）。
		// 注意 CLI 2.x 取值为 manual（无 "default"；auto 会由分类器静默放行）。
		"--permission-mode", "manual",
	}
	// multi-turn：等号形式绑定（空格形式会被 CLI 解析成独立 flag，SDK 同样如此处理）
	if req.PrevSession != "" {
		args = append(args, "--resume="+req.PrevSession)
	}
	cmd := exec.CommandContext(ctx, c.Binary, args...)
	cmd.Dir = c.WorkDir
	// SDK 环境约定：剥掉 CLAUDECODE，打上 SDK 标识
	env := []string{}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CLAUDECODE=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"CLAUDE_CODE_ENTRYPOINT=sdk-py",
		"CLAUDE_AGENT_SDK_VERSION=0.3.301-bridge",
	)
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start claude: %w", err)
	}
	c.mu.Lock()
	c.running[req.TaskID] = cmd
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.running, req.TaskID)
		c.mu.Unlock()
	}()

	writeStdin := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		b = append(b, '\n')
		_, err = stdin.Write(b)
		return err
	}

	// initialize 握手（SDK: control_request {subtype:"initialize"}）
	initID := core.NewID("init_")
	if err := writeStdin(map[string]any{
		"type":       "control_request",
		"request_id": initID,
		"request":    map[string]any{"subtype": "initialize", "hooks": nil},
	}); err != nil {
		cmd.Process.Kill()
		return fmt.Errorf("initialize write: %w", err)
	}

	sink(core.RunEvent{Kind: "status", Status: core.StateWorking, Message: "claude is thinking..."})

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	var (
		fallbackText strings.Builder // whole-message fallback (no partials)
		streamedText strings.Builder // text already delivered via deltas
		inReasoning  bool
		resultSeen   bool
		cliSession   string
	)
	closeReasoning := func() {
		if inReasoning {
			sink(core.RunEvent{Kind: "reasoning_end"})
			inReasoning = false
		}
	}

	promptSent := false
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m claudeStreamMsg
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		switch m.Type {
		case "system":
			if m.Subtype == "init" && m.SessionID != "" {
				cliSession = m.SessionID
				if req.PrevSession == "" && req.SaveSessionID != nil {
					req.SaveSessionID(m.SessionID)
				}
			}

		case "control_response":
			// initialize ack — nothing to do (errors land as subtype=error)

		case "control_request":
			rid := m.RequestID
			var cr controlRequest
			json.Unmarshal(m.Request, &cr)
			log.Printf("[claude:%s] control_request rid=%s subtype=%s tool=%s", req.TaskID, rid, cr.Subtype, cr.ToolName)
			switch cr.Subtype {
			case "can_use_tool":
				// 真·中途 HITL：映射为 bridge_hitl，等人类决策后回 control_response
				toolName := cr.ToolName
				inputBrief := []byte(cr.Input)
				if len(inputBrief) > 400 {
					inputBrief = inputBrief[:400]
				}
				prompt := fmt.Sprintf("允许执行工具 %s 吗？\n入参: %s", toolName, inputBrief)
				sink(core.RunEvent{Kind: "input", Input: &core.RequiredInput{
					InputID: rid, Type: "approval", Prompt: prompt,
				}})
				sink(core.RunEvent{Kind: "toolcall", ToolCallID: rid, ToolCallName: "bridge_hitl",
					ToolArgs: core.MustJSON(map[string]any{
						"input_id": rid, "type": "approval", "prompt": prompt,
						"tool_name": toolName,
					})})
				sink(core.RunEvent{Kind: "pause"})

				var resp core.InputResponse
				select {
				case resp = <-req.InputCh:
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(10 * time.Minute):
					_ = writeStdin(map[string]any{
						"type": "control_response",
						"response": map[string]any{
							"subtype": "error", "request_id": rid,
							"error": "hitl_timeout",
						},
					})
					return fmt.Errorf("hitl_timeout")
				}

				if resp.Action == "reject" {
					_ = writeStdin(map[string]any{
						"type": "control_response",
						"response": map[string]any{
							"subtype": "success", "request_id": rid,
							"response": map[string]any{
								"behavior": "deny",
								"message":  "用户在 bridge 审批中拒绝了此操作",
							},
						},
					})
				} else {
					_ = writeStdin(map[string]any{
						"type": "control_response",
						"response": map[string]any{
							"subtype": "success", "request_id": rid,
							"response": map[string]any{
								"behavior":     "allow",
								"updatedInput": cr.Input,
							},
						},
					})
				}

			default:
				// 其他控制请求：回错误，保持 CLI 不阻塞
				_ = writeStdin(map[string]any{
					"type": "control_response",
					"response": map[string]any{
						"subtype": "error", "request_id": rid,
						"error": "unsupported control subtype: " + cr.Subtype,
					},
				})
			}

		case "stream_event":
			ev := m.Event
			switch ev.Type {
			case "content_block_delta":
				switch ev.Delta.Type {
				case "text_delta":
					if ev.Delta.Text != "" {
						closeReasoning()
						streamedText.WriteString(ev.Delta.Text)
						sink(core.RunEvent{Kind: "text", Text: ev.Delta.Text})
					}
				case "thinking_delta":
					if ev.Delta.Thinking != "" {
						if !inReasoning {
							inReasoning = true
							sink(core.RunEvent{Kind: "reasoning_start"})
						}
						sink(core.RunEvent{Kind: "reasoning", Text: ev.Delta.Thinking})
					}
				}
			case "content_block_start":
				if ev.ContentBlock.Type == "text" && ev.ContentBlock.Text != "" && streamedText.Len() == 0 {
					closeReasoning()
					streamedText.WriteString(ev.ContentBlock.Text)
					sink(core.RunEvent{Kind: "text", Text: ev.ContentBlock.Text})
				}
			}

		case "assistant":
			if streamedText.Len() > 0 {
				continue
			}
			closeReasoning()
			for _, blk := range m.Message.Content {
				if blk.Type == "text" && blk.Text != "" {
					fallbackText.WriteString(blk.Text)
				}
			}

		case "result":
			resultSeen = true
			if streamedText.Len() == 0 && fallbackText.Len() == 0 && m.Result != "" {
				fallbackText.WriteString(m.Result)
			}

		case "error":
			// CLI-level error object on stdout
			if m.Error.Message != "" {
				return fmt.Errorf("claude error: %s", m.Error.Message)
			}
		}

		// 握手完成后（收到任意首条 CLI 消息即可认为 init 已就绪），
		// 立即投递 prompt。实际在 init ack 或第一条消息后都可发送。
		if !promptSent && (m.Type == "system" || m.Type == "control_response") {
			if err := writeStdin(map[string]any{
				"type":               "user",
				"message":            map[string]any{"role": "user", "content": req.Prompt},
				"parent_tool_use_id": nil,
				"session_id":         cliSession,
			}); err != nil {
				cmd.Process.Kill()
				return fmt.Errorf("prompt write: %w", err)
			}
			promptSent = true
			log.Printf("[claude:%s] prompt sent (session=%s)", req.TaskID, cliSession)
		}

		if resultSeen {
			break
		}
	}
	closeReasoning()

	// 关闭 stdin → CLI 收尾；等待进程退出
	stdin.Close()

	// fallback：整条消息到达（旧 CLI 无 partial）→ 软切片流式输出
	if streamedText.Len() == 0 && fallbackText.Len() > 0 {
		if err := core.StreamText(ctx, sink, fallbackText.String(), 12, 18*time.Millisecond); err != nil {
			return err
		}
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("claude exited: %s", msg)
	}
	sink(core.RunEvent{Kind: "done"})
	return nil
}
