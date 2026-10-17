// Package acp: 通用 ACP（Agent Client Protocol v1）子进程适配器。
//
// 一个实现服务所有 ACP coding agent：
//
//	claude-code → npx @agentclientprotocol/claude-agent-acp
//	openclaw    → openclaw acp（内置）
//	codex/gemini/... → 各自的 ACP launcher（注册一行即可）
//
// 协议：JSON-RPC 2.0 over stdio（NDJSON 帧）
//
//	initialize → session/new（或 session/load 续会话）
//	session/prompt 期间 agent 推 session/update（thought/message chunk）
//	agent→client 请求 session/request_permission → client 回 {outcome:{outcome:"selected",optionId}}
package acp

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

	"xagent-bridge/internal/core"
	"xagent-bridge/internal/runtimes/sesscache"
)

type Adapter struct {
	name             string
	command          []string
	workDir          string
	*sesscache.Cache // 会话缓存：提升为 SessionStore/SessionHistory 实现
}

// NewAdapter 校验 command 可执行后构造；缺失返回 nil。
func NewAdapter(name string, command []string, workDir, dataDir string) *Adapter {
	if len(command) == 0 {
		return nil
	}
	if _, err := exec.LookPath(command[0]); err != nil {
		return nil
	}
	return &Adapter{name: name, command: command, workDir: workDir, Cache: sesscache.Open(dataDir, name)}
}

// core.SessionBinder：thread → acp sessionId。
func (a *Adapter) SessionHandle(thread string) string      { return a.Cache.Handle(thread) }
func (a *Adapter) SaveSessionHandle(thread, handle string) { a.Cache.SetHandle(thread, handle) }

func (a *Adapter) ID() string { return a.name }

func (a *Adapter) Name() string {
	switch a.name {
	case "claude-code":
		return "Claude Code (ACP)"
	case "openclaw":
		return "OpenClaw (ACP)"
	}
	return a.name + " (ACP)"
}

func (a *Adapter) Health() (string, string) {
	if _, err := exec.LookPath(a.command[0]); err != nil {
		return "offline", "acp"
	}
	return "online", "acp"
}

func (a *Adapter) Capabilities() map[string]bool {
	return map[string]bool{
		"agui": true, "streaming": true, "hitl": true, "sessions": true,
		"skills": true, "mcp": true, "files": false,
	}
}

func (a *Adapter) Cancel() {}

// ---- JSON-RPC 客户端 ----

type rpcMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("acp error %d: %s", e.Code, e.Message) }

type acpClient struct {
	stdin     *bufio.Writer
	writeMu   sync.Mutex
	pendMu    sync.Mutex
	pending   map[string]chan rpcMsg
	nextID    int
	updateMu  sync.Mutex
	onUpdate  func(params json.RawMessage)
	closed    chan struct{}
	closeOnce sync.Once
	tag       string // 日志标签
}

func (c *acpClient) setOnUpdate(fn func(json.RawMessage)) {
	c.updateMu.Lock()
	c.onUpdate = fn
	c.updateMu.Unlock()
}

func (c *acpClient) emitUpdate(params json.RawMessage) {
	c.updateMu.Lock()
	fn := c.onUpdate
	c.updateMu.Unlock()
	if fn != nil {
		fn(params)
	}
}

func (c *acpClient) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if os.Getenv("XAGENT_ACP_DEBUG") != "" {
		log.Printf("[acp:%s] >> %s", c.tag, truncate(string(b), 200))
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(b); err != nil {
		return err
	}
	return c.stdin.Flush()
}

func (c *acpClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.pendMu.Lock()
	c.nextID++
	sid := fmt.Sprintf("%d", c.nextID)
	ch := make(chan rpcMsg, 1)
	c.pending[sid] = ch
	c.pendMu.Unlock()
	defer func() {
		c.pendMu.Lock()
		delete(c.pending, sid)
		c.pendMu.Unlock()
	}()
	if err := c.write(map[string]any{
		"jsonrpc": "2.0", "id": sid, "method": method, "params": params,
	}); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, fmt.Errorf("acp process exited")
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Result, resp.Error
		}
		return resp.Result, nil
	}
}

func (c *acpClient) respond(id json.RawMessage, result any) {
	_ = c.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// ---- Run：一次 ACP prompt turn（每 turn 一个子进程） ----

func (a *Adapter) Run(ctx context.Context, req core.AdapterRequest, sink func(core.RunEvent)) error {
	sink(core.RunEvent{Kind: "status", Status: core.StateWorking, Message: "launching " + a.name + " (acp)..."})

	cmd := exec.CommandContext(ctx, a.command[0], a.command[1:]...)
	cmd.Dir = a.workDir
	cmd.Env = os.Environ()
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
		return fmt.Errorf("start acp agent: %w", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	client := &acpClient{
		stdin:   bufio.NewWriter(stdin),
		pending: map[string]chan rpcMsg{},
		closed:  make(chan struct{}),
		tag:     a.name,
	}

	reasoningOpen := false
	closeReasoning := func() {
		if reasoningOpen {
			sink(core.RunEvent{Kind: "reasoning_end"})
			reasoningOpen = false
		}
	}
	// 部分 agent 短回复单 chunk 到达 → 大块软切片保打字机效果
	emitText := func(ctx context.Context, s string) {
		if s == "" {
			return
		}
		closeReasoning()
		if len([]rune(s)) > 24 {
			_ = core.StreamText(ctx, sink, s, 10, 12*1_000_000) // 12ms
		} else {
			sink(core.RunEvent{Kind: "text", Text: s})
		}
	}
	// session/update → 语义事件（load 回放时可临时替换为空实现）
	client.setOnUpdate(func(params json.RawMessage) {
		var p struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				Content       struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		_ = json.Unmarshal(params, &p)
		switch p.Update.SessionUpdate {
		case "agent_thought_chunk":
			if !reasoningOpen {
				reasoningOpen = true
				sink(core.RunEvent{Kind: "reasoning_start"})
			}
			sink(core.RunEvent{Kind: "reasoning", Text: p.Update.Content.Text})
		case "agent_message_chunk":
			closeReasoning()
			emitText(ctx, p.Update.Content.Text)
		}
	})

	// reader：响应路由 + agent→client 权限请求
	go func() {
		defer client.closeOnce.Do(func() { close(client.closed) })
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			line := sc.Text()
			if os.Getenv("XAGENT_ACP_DEBUG") != "" {
				log.Printf("[acp:%s] << %s", a.name, truncate(line, 200))
			}
			var msg rpcMsg
			if json.Unmarshal([]byte(line), &msg) != nil {
				continue
			}
			switch {
			case msg.Method == "session/update":
				client.emitUpdate(msg.Params)
			case msg.ID != nil && msg.Method != "":
				if msg.Method == "session/request_permission" {
					a.handlePermission(ctx, client, msg.ID, msg.Params, req.InputCh, sink)
				} else {
					client.respond(msg.ID, map[string]any{})
				}
			case msg.ID != nil:
				// json.RawMessage 的 id 含引号，需还原为字符串键
				var rid string
				_ = json.Unmarshal(msg.ID, &rid)
				client.pendMu.Lock()
				ch := client.pending[rid]
				client.pendMu.Unlock()
				if ch != nil {
					ch <- msg
				}
			}
		}
	}()

	if _, err := client.request(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}},
	}); err != nil {
		return fmt.Errorf("acp initialize: %w (%s)", err, firstLine(stderr.String()))
	}
	log.Printf("[acp:%s] initialize ok, prev=%q", a.name, req.PrevSession)

	sink(core.RunEvent{Kind: "status", Status: core.StateWorking, Message: a.name + " is thinking..."})
	log.Printf("[acp:%s] status sinked", a.name)

	// 会话：resume（load 回放丢弃）或 new
	sessionID := req.PrevSession
	if sessionID != "" {
		client.setOnUpdate(func(json.RawMessage) {})
		if _, err := client.request(ctx, "session/load", map[string]any{"sessionId": sessionID}); err != nil {
			sessionID = "" // 会话丢失 → 新建
		}
		// 恢复 streaming handler
		client.setOnUpdate(func(params json.RawMessage) {
			var p struct {
				Update struct {
					SessionUpdate string `json:"sessionUpdate"`
					Content       struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"update"`
			}
			_ = json.Unmarshal(params, &p)
			switch p.Update.SessionUpdate {
			case "agent_thought_chunk":
				if !reasoningOpen {
					reasoningOpen = true
					sink(core.RunEvent{Kind: "reasoning_start"})
				}
				sink(core.RunEvent{Kind: "reasoning", Text: p.Update.Content.Text})
			case "agent_message_chunk":
				closeReasoning()
				emitText(ctx, p.Update.Content.Text)
			}
		})
	}
	if sessionID == "" {
		// openclaw 的 ACP schema 要求 mcpServers 必填（可为空数组）
		res, err := client.request(ctx, "session/new", map[string]any{
			"cwd":        a.workDir,
			"mcpServers": []any{},
		})
		if err != nil {
			return fmt.Errorf("acp session/new: %w", err)
		}
		var created struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(res, &created)
		if created.SessionID == "" {
			return fmt.Errorf("acp session/new returned no sessionId: %s", truncate(string(res), 160))
		}
		sessionID = created.SessionID
		if req.SaveSessionID != nil {
			req.SaveSessionID(sessionID)
		}
	}

	res, err := client.request(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": req.Prompt}},
	})
	closeReasoning()
	if err != nil {
		return fmt.Errorf("acp session/prompt: %w", err)
	}
	var turn struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(res, &turn)
	switch turn.StopReason {
	case "cancelled":
		return ctx.Err()
	default: // end_turn / max_tokens / ""
		sink(core.RunEvent{Kind: "done"})
		return nil
	}
}

// handlePermission：ACP 权限请求 → bridge HITL → 回 selected/cancelled。
// 在 reader goroutine 内阻塞等待人类决策（agent 同时被阻塞，无并发消息）。
func (a *Adapter) handlePermission(ctx context.Context, c *acpClient, id json.RawMessage, params json.RawMessage, inputCh chan core.InputResponse, sink func(core.RunEvent)) {
	var p struct {
		ToolCall struct {
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(params, &p)
	title := p.ToolCall.Title
	if title == "" {
		title = "工具执行请求"
	}
	prompt := fmt.Sprintf("允许执行工具 %s 吗？", title)
	sink(core.RunEvent{Kind: "input", Input: &core.RequiredInput{
		InputID: p.ToolCall.ToolCallID, Type: "approval", Prompt: prompt,
	}})
	sink(core.RunEvent{Kind: "toolcall", ToolCallID: p.ToolCall.ToolCallID, ToolCallName: "bridge_hitl",
		ToolArgs: core.MustJSON(map[string]any{
			"input_id": p.ToolCall.ToolCallID, "type": "approval",
			"prompt": prompt, "tool_name": title,
		})})
	sink(core.RunEvent{Kind: "pause"})

	var resp core.InputResponse
	select {
	case <-ctx.Done():
		c.respond(id, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
		return
	case resp = <-inputCh:
	}
	chosen := ""
	for _, o := range p.Options {
		kind := strings.ToLower(o.Kind)
		if resp.Action != "reject" && strings.HasPrefix(kind, "allow") {
			chosen = o.OptionID
			break
		}
		if resp.Action == "reject" && (strings.HasPrefix(kind, "reject") || kind == "cancel") {
			chosen = o.OptionID
			break
		}
	}
	if chosen == "" && len(p.Options) > 0 {
		chosen = p.Options[0].OptionID
	}
	c.respond(id, map[string]any{
		"outcome": map[string]any{"outcome": "selected", "optionId": chosen},
	})
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---- skills：通用目录扫描（覆盖 claude 与 openclaw 布局） ----

func (a *Adapter) ListSkills(workspace string) ([]core.SkillItem, error) {
	home, _ := os.UserHomeDir()
	roots := []struct{ dir, src string }{
		{filepath.Join(workspace, "skills"), "workspace"},
		{filepath.Join(workspace, ".agents", "skills"), "workspace"},
		{filepath.Join(workspace, ".claude", "skills"), "workspace"},
		{filepath.Join(home, ".agents", "skills"), "user"},
		{filepath.Join(home, ".claude", "skills"), "user"},
	}
	seen := map[string]bool{}
	var out []core.SkillItem
	for _, r := range roots {
		entries, err := os.ReadDir(r.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || seen[e.Name()] {
				continue
			}
			body, err := os.ReadFile(filepath.Join(r.dir, e.Name(), "SKILL.md"))
			if err != nil {
				continue
			}
			seen[e.Name()] = true
			name, desc := core.ParseSkillFrontmatter(string(body))
			if name == "" {
				name = e.Name()
			}
			out = append(out, core.SkillItem{
				ID: e.Name(), Name: name, Description: desc,
				Enabled: true, Source: r.src,
			})
		}
	}
	return out, nil
}

// ---- mcp：读取常见配置文件（只读，覆盖两种布局） ----

func (a *Adapter) ListMCP(workspace string) ([]core.MCPServerItem, error) {
	home, _ := os.UserHomeDir()
	paths := []string{
		filepath.Join(workspace, ".mcp.json"),
		filepath.Join(home, ".openclaw", "openclaw.json"),
	}
	seen := map[string]bool{}
	out := []core.MCPServerItem{}
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cfg struct {
			MCPServers map[string]struct {
				Command string            `json:"command"`
				Args    []string          `json:"args"`
				Env     map[string]string `json:"env"`
				URL     string            `json:"url"`
				Type    string            `json:"type"`
			} `json:"mcpServers"`
			MCP struct {
				Servers map[string]struct {
					Command string            `json:"command"`
					Args    []string          `json:"args"`
					Env     map[string]string `json:"env"`
					URL     string            `json:"url"`
					Type    string            `json:"type"`
				} `json:"servers"`
			} `json:"mcp"`
		}
		if err := json.Unmarshal(body, &cfg); err != nil {
			continue
		}
		servers := cfg.MCPServers
		if len(servers) == 0 {
			servers = cfg.MCP.Servers
		}
		for id, srv := range servers {
			if seen[id] {
				continue
			}
			seen[id] = true
			transport := "stdio"
			cmd := strings.TrimSpace(srv.Command + " " + strings.Join(srv.Args, " "))
			if srv.URL != "" {
				transport = srv.Type
				if transport == "" {
					transport = "http"
				}
				cmd = srv.URL
			}
			env := map[string]string{}
			for k := range srv.Env {
				env[k] = "***"
			}
			out = append(out, core.MCPServerItem{
				ID: id, Name: id, Transport: transport, Command: cmd,
				Enabled: true, Status: "unknown", Env: env,
			})
		}
	}
	return out, nil
}
