// Package core 定义 bridge 的共享契约：适配器接口、任务/会话模型、
// 事件投影器。所有 runtime 包实现 Adapter 接口即可接入。
package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---- 事件与适配器契约 ----

type TaskState string

const (
	StateSubmitted     TaskState = "submitted"
	StateWorking       TaskState = "working"
	StateInputRequired TaskState = "input-required"
	StateCompleted     TaskState = "completed"
	StateFailed        TaskState = "failed"
	StateCanceled      TaskState = "canceled"
	StateRejected      TaskState = "rejected"
)

func (s TaskState) Terminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateCanceled || s == StateRejected
}

type RequiredInput struct {
	InputID   string   `json:"input_id"`
	Type      string   `json:"type"` // approval | choice | text | form
	Prompt    string   `json:"prompt"`
	Options   []string `json:"options,omitempty"`
	Fields    []any    `json:"fields,omitempty"`
	ExpiresAt string   `json:"expires_at,omitempty"`
}

type Artifact struct {
	ArtifactID string `json:"artifact_id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Index      int    `json:"index"`
	Content    string `json:"content"`
}

// RunEvent 是适配器上报的语义事件；投影器/协议层负责翻译成线上格式。
type RunEvent struct {
	Kind string // status | text | reasoning_start | reasoning | reasoning_end | input | toolcall | pause | done

	Status  TaskState
	Message string

	Text string

	ToolCallID   string
	ToolCallName string
	ToolArgs     string

	Input *RequiredInput

	Err error
}

type InputResponse struct {
	InputID string
	Action  string
	Data    map[string]any
}

type AdapterRequest struct {
	TaskID    string
	ContextID string
	Prompt    string
	// InputCh is bridge-owned (buffered); HITL responses are delivered here.
	InputCh chan InputResponse
	// PrevSession is the runtime-native session handle for this thread, if any.
	PrevSession string
	// SaveSessionID persists the runtime-native session id for future turns.
	SaveSessionID func(id string)
}

// Adapter 是 RuntimeAdapter 契约（spec §12）：只上报语义事件，不关心线上格式。
type Adapter interface {
	ID() string
	Name() string
	Health() (status string, version string)
	Capabilities() map[string]bool
	// Run 执行一轮 prompt；sink 必须串行调用；需要 HITL 时上报 input 事件
	// 并阻塞读 req.InputCh。
	Run(ctx context.Context, req AdapterRequest, sink func(RunEvent)) error
	Cancel()
}

type AdapterRuntime struct {
	ID        string
	Name      string
	Status    string
	Version   string
	Workspace string
	Adapter   Adapter
}

// ---- 管理面资源与接口 ----

type SkillItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	Source      string `json:"source"`
}

type MCPServerItem struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	Command   string            `json:"command,omitempty"`
	Enabled   bool              `json:"enabled"`
	Status    string            `json:"status"`
	Env       map[string]string `json:"env,omitempty"`
}

type SkillsAdmin interface {
	InstallSkill(workspace, name, content string) (SkillItem, error)
	SetSkillEnabled(workspace, id string, enabled bool) (SkillItem, error)
	RemoveSkill(workspace, id string) error
}

type MCPAdmin interface {
	UpsertMCP(workspace string, item MCPServerItem, env map[string]string) (MCPServerItem, error)
	RemoveMCP(workspace, id string) error
}

type SkillsAdminProvider interface{ Admin() SkillsAdmin }
type MCPAdminProvider interface{ MCPAdmin() MCPAdmin }

type SkillsProvider interface {
	ListSkills(workspace string) ([]SkillItem, error)
}

type MCPProvider interface {
	ListMCP(workspace string) ([]MCPServerItem, error)
}

// ---- 会话能力（adapter 层抽象；数据来源由各 adapter 自决） ----

type SessionMeta struct {
	ID        string `json:"session_id"`
	Title     string `json:"title"`
	TaskCount int    `json:"task_count"`
	UpdatedAt string `json:"updated_at"`
}

type HistoryMsg struct {
	Role        string       `json:"role"`
	Content     string       `json:"content"`
	Thinking    string       `json:"thinking,omitempty"`
	TaskID      string       `json:"task_id,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	CreatedAt   string       `json:"created_at"`
}

// Attachment 用户上传的文件（落盘 bridge data/uploads/<rt>/<thread>/）。
type Attachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"` // 绝对路径，agent 用自身工具直读
	Size int64  `json:"size"`
}

// InjectAttachments 把上传文件清单注入 prompt 尾部（仅给 adapter；
// 历史存原始文本 + 附件元数据，注入文本不落库）。
func InjectAttachments(prompt string, atts []Attachment) string {
	if len(atts) == 0 {
		return prompt
	}
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\n[用户上传了以下文件，请直接用工具读取并分析]\n")
	for _, a := range atts {
		name := a.Name
		if name == "" {
			name = a.ID
		}
		b.WriteString("- " + name + " (" + HumanSize(a.Size) + "): " + a.Path + "\n")
	}
	return b.String()
}

// HumanSize 人类可读大小。
func HumanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/(1<<20)), ".0") + "MB"
	case n >= 1<<10:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/(1<<10)), ".0") + "KB"
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// SessionStore：会话列表与标题（产品语义；ACP adapter 可缓存自 agent 的 session_info）。
type SessionStore interface {
	// EnsureSession 确保会话存在；seedTitle 仅在新会话（标题为空）时采用。
	EnsureSession(sessionID, seedTitle string) error
	ListSessions() ([]SessionMeta, error)
	SetTitle(sessionID, title string) error
}

// SessionHistory：会话消息读写（ACP adapter 写透缓存 + agent 回放为权威源）。
type SessionHistory interface {
	ListMessages(sessionID string) ([]HistoryMsg, error)
	AppendMessage(sessionID string, m HistoryMsg) error
}

// SessionBinder：thread → runtime 原生会话句柄（如 acp sessionId）的映射，adapter 私有。
type SessionBinder interface {
	SessionHandle(thread string) string
	SaveSessionHandle(thread, handle string)
}

// skills/mcp 管理语义错误（server 侧 errors.Is 判断）。
var (
	ErrSkillExists   = errSentinel("skill_exists")
	ErrSkillNotFound = errSentinel("skill_not_found")
)

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

// ---- 通用工具 ----

func NewID(prefix string) string {
	var b [8]byte
	rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

func MustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func Chunkify(s string, n int) []string {
	rs := []rune(s)
	if len(rs) == 0 {
		return nil
	}
	var out []string
	for i := 0; i < len(rs); i += n {
		j := i + n
		if j > len(rs) {
			j = len(rs)
		}
		out = append(out, string(rs[i:j]))
	}
	return out
}

// StreamText 将整段文本按小片延迟上报，制造渐进输出效果。
func StreamText(ctx context.Context, sink func(RunEvent), s string, n int, delay time.Duration) error {
	for _, chunk := range Chunkify(s, n) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		sink(RunEvent{Kind: "text", Text: chunk})
	}
	return nil
}

// ParseSkillFrontmatter 读取 SKILL.md YAML front 的 name/description。
func ParseSkillFrontmatter(body string) (name, desc string) {
	if !strings.HasPrefix(body, "---") {
		return "", ""
	}
	rest := strings.TrimPrefix(body, "---")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", ""
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "name:"):
			name = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "name:")), `"'`)
		case strings.HasPrefix(line, "description:"):
			desc = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "description:")), `"'`)
		}
	}
	return name, desc
}
