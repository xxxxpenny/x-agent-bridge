// openclaw.go: OpenClaw runtime adapter —— 纯 Go Gateway WS 客户端。
//
// 流式 + HITL 走 Gateway WS 协议（A2A channel 无 streaming/无法回喂审批）：
//
//	chat.send → agent 事件（thinking/assistant delta、lifecycle）
//	exec.approval.requested → bridge_hitl HITL → exec.approval.resolve(allow-once|deny)
//
// 多轮：session-key = agent:main:<thread_id>，Gateway 侧持久化。
package openclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"xagent-bridge/internal/core"
	"xagent-bridge/internal/runtimes/sesscache"
)

type Adapter struct {
	Binary           string // openclaw CLI（pairing 兜底）
	DataDir          string // 设备身份/token 存储目录
	*sesscache.Cache        // 会话缓存：SessionStore/SessionHistory
}

// NewAdapter 构造适配器；gateway 未运行时仍返回实例（Health 如实上报 offline）。
func NewAdapter(dataDir string) *Adapter {
	bin, _ := exec.LookPath("openclaw")
	devDir := filepath.Join(dataDir, "data", "openclaw-device")
	return &Adapter{
		Binary: bin, DataDir: devDir,
		Cache: sesscache.Open(dataDir, "openclaw-legacy"),
	}
}

// core.SessionBinder：session-key 由 thread 确定性推导（agent:main:<thread>），
// 无需存储；handle 仅作接口一致性回显。
func (a *Adapter) SessionHandle(thread string) string      { return openclawSessionKey(thread) }
func (a *Adapter) SaveSessionHandle(thread, handle string) {}

func (a *Adapter) ID() string   { return "openclaw" }
func (a *Adapter) Name() string { return "OpenClaw Gateway" }

func (a *Adapter) Health() (string, string) {
	port := os.Getenv("OPENCLAW_GATEWAY_PORT")
	if port == "" {
		port = "18789"
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 2*time.Second)
	if err != nil {
		return "offline", "ws"
	}
	conn.Close()
	return "online", "ws"
}

func (a *Adapter) Capabilities() map[string]bool {
	return map[string]bool{
		"agui": true, "streaming": true, "hitl": true, "sessions": true,
		"skills": true, "mcp": true, "files": false,
	}
}

func (a *Adapter) Cancel() {}

func gatewayToken() string {
	if t := os.Getenv("OPENCLAW_GATEWAY_TOKEN"); t != "" {
		return t
	}
	b, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".openclaw", "openclaw.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		Gateway struct {
			Auth struct {
				Token string `json:"token"`
			} `json:"auth"`
		} `json:"gateway"`
	}
	_ = json.Unmarshal(b, &cfg)
	return cfg.Gateway.Auth.Token
}

// openclawSessionKey：thread → OpenClaw 会话 key 的确定性映射。
func openclawSessionKey(threadID string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, threadID)
	return "agent:main:" + safe
}

func gatewayURL() string {
	if u := os.Getenv("OPENCLAW_GATEWAY_URL"); u != "" {
		return u
	}
	port := os.Getenv("OPENCLAW_GATEWAY_PORT")
	if port == "" {
		port = "18789"
	}
	return "ws://127.0.0.1:" + port + "/"
}

func (a *Adapter) Run(ctx context.Context, req core.AdapterRequest, sink func(core.RunEvent)) error {
	sink(core.RunEvent{Kind: "status", Status: core.StateWorking, Message: "connecting openclaw gateway..."})

	sessionKey := req.PrevSession
	if sessionKey == "" {
		sessionKey = openclawSessionKey(req.ContextID)
		if req.SaveSessionID != nil {
			req.SaveSessionID(sessionKey)
		}
	}

	events := make(chan gwEvent, 256)
	gc, _, err := dialGateway(ctx, gatewayURL(), gatewayToken(), a.DataDir, func(event string, payload json.RawMessage) {
		events <- gwEvent{name: event, payload: payload}
	})
	if err != nil {
		return fmt.Errorf("openclaw gateway 连接失败: %w", err)
	}
	defer gc.Close()

	sink(core.RunEvent{Kind: "status", Status: core.StateWorking, Message: "openclaw is thinking..."})
	res, err := gc.Request(ctx, "chat.send", map[string]any{
		"sessionKey":     sessionKey,
		"message":        req.Prompt,
		"idempotencyKey": core.NewID("idem_"),
		"timeoutMs":      600000,
	})
	if err != nil {
		return fmt.Errorf("chat.send: %w", err)
	}
	var sendRes struct {
		RunID string `json:"runId"`
	}
	_ = json.Unmarshal(res, &sendRes)
	if sendRes.RunID == "" {
		return fmt.Errorf("chat.send 未返回 runId: %s", truncate(string(res), 200))
	}
	runID := sendRes.RunID

	var (
		inReasoning bool
		gotText     bool
	)
	closeReasoning := func() {
		if inReasoning {
			sink(core.RunEvent{Kind: "reasoning_end"})
			inReasoning = false
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-events:
			switch ev.name {
			case "exec.approval.requested":
				var p struct {
					ID      string `json:"id"`
					Request struct {
						Command string   `json:"command"`
						Cwd     string   `json:"cwd"`
						RunID   string   `json:"runId"`
						Allowed []string `json:"allowedDecisions"`
					} `json:"request"`
				}
				_ = json.Unmarshal(ev.payload, &p)
				if p.Request.RunID != "" && p.Request.RunID != runID {
					continue // 其他会话的审批不归本 run
				}
				closeReasoning()
				prompt := fmt.Sprintf("允许执行工具 exec 吗？\n入参: {\"command\": %q, \"cwd\": %q}", p.Request.Command, p.Request.Cwd)
				sink(core.RunEvent{Kind: "input", Input: &core.RequiredInput{
					InputID: p.ID, Type: "approval", Prompt: prompt,
				}})
				sink(core.RunEvent{Kind: "toolcall", ToolCallID: p.ID, ToolCallName: "bridge_hitl",
					ToolArgs: core.MustJSON(map[string]any{
						"input_id": p.ID, "type": "approval", "prompt": prompt,
						"tool_name": "exec", "command": p.Request.Command,
					})})
				sink(core.RunEvent{Kind: "pause"})

				var resp core.InputResponse
				select {
				case resp = <-req.InputCh:
				case <-ctx.Done():
					return ctx.Err()
				}
				decision := "allow-once"
				if resp.Action == "reject" {
					decision = "deny"
				}
				if _, err := gc.Request(ctx, "exec.approval.resolve", map[string]any{
					"id": p.ID, "decision": decision,
				}); err != nil {
					return fmt.Errorf("exec.approval.resolve: %w", err)
				}

			case "agent":
				var p struct {
					RunID  string `json:"runId"`
					Stream string `json:"stream"`
					Data   struct {
						Delta         string `json:"delta"`
						Phase         string `json:"phase"`
						StopReason    string `json:"stopReason"`
						Aborted       bool   `json:"aborted"`
						TerminalReply struct {
							Text string `json:"text"`
						} `json:"terminalReply"`
					} `json:"data"`
				}
				_ = json.Unmarshal(ev.payload, &p)
				if p.RunID != runID {
					continue
				}
				switch p.Stream {
				case "thinking":
					if p.Data.Delta != "" && !gotText {
						if !inReasoning {
							inReasoning = true
							sink(core.RunEvent{Kind: "reasoning_start"})
						}
						sink(core.RunEvent{Kind: "reasoning", Text: p.Data.Delta})
					}
				case "assistant":
					if p.Data.Delta != "" {
						closeReasoning()
						gotText = true
						// Gateway 单事件整段投递 → 软切片保打字机效果
						if len([]rune(p.Data.Delta)) > 24 {
							_ = core.StreamText(ctx, sink, p.Data.Delta, 10, 12*time.Millisecond)
						} else {
							sink(core.RunEvent{Kind: "text", Text: p.Data.Delta})
						}
					}
				case "lifecycle":
					if p.Data.Phase == "end" {
						closeReasoning()
						if p.Data.Aborted || p.Data.StopReason == "aborted" {
							return ctx.Err()
						}
						sink(core.RunEvent{Kind: "done"})
						return nil
					}
				}
			}
		}
	}
}

type gwEvent struct {
	name    string
	payload json.RawMessage
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---- skills：扫描 OpenClaw 的 SKILL.md 目录（只读） ----

func (a *Adapter) ListSkills(workspace string) ([]core.SkillItem, error) {
	home, _ := os.UserHomeDir()
	roots := []struct{ dir, src string }{
		{filepath.Join(workspace, "skills"), "workspace"},
		{filepath.Join(workspace, ".agents", "skills"), "workspace"},
		{filepath.Join(home, ".agents", "skills"), "user"},
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

// ---- mcp：读取 ~/.openclaw/openclaw.json 的 mcp.servers（只读） ----

func (a *Adapter) ListMCP(workspace string) ([]core.MCPServerItem, error) {
	home, _ := os.UserHomeDir()
	body, err := os.ReadFile(filepath.Join(home, ".openclaw", "openclaw.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg struct {
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
		return nil, fmt.Errorf("bad openclaw.json: %w", err)
	}
	out := []core.MCPServerItem{}
	for id, srv := range cfg.MCP.Servers {
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
	return out, nil
}
