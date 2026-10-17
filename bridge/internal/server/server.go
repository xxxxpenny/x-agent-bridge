// Package server: HTTP 面（REST 管理面 + AG-UI 聊天面）。
package server

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"xagent-bridge/internal/core"
	"xagent-bridge/internal/llmgw"
)

type Server struct {
	store     *core.Store
	adapters  map[string]*core.AdapterRuntime
	llm       *llmgw.Service
	token     string // 非空时启用 Bearer 校验
	dataDir   string // uploads 落盘根目录
	startedAt time.Time
}

// New 创建 HTTP 服务；token 非空时全接口（除 health/OPTIONS）要求
// Authorization: Bearer <token>（或 ?access_token=）。
func New(store *core.Store, adapters map[string]*core.AdapterRuntime, llm *llmgw.Service, token, dataDir string) *Server {
	return &Server{store: store, adapters: adapters, llm: llm, token: token, dataDir: dataDir, startedAt: time.Now()}
}

// Handler 返回完整路由（含 CORS + 访问日志）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/system/health", s.handleHealth)
	mux.HandleFunc("GET /api/v1/runtimes", s.handleRuntimes)

	mux.HandleFunc("POST /api/v1/{rt}/ag-ui", func(w http.ResponseWriter, r *http.Request) {
		s.handleAGUI(w, r, r.PathValue("rt"))
	})
	mux.HandleFunc("GET /api/v1/{rt}/tasks", func(w http.ResponseWriter, r *http.Request) {
		s.handleListTasks(w, r, r.PathValue("rt"))
	})
	mux.HandleFunc("POST /api/v1/{rt}/tasks", func(w http.ResponseWriter, r *http.Request) {
		s.handlePostTask(w, r, r.PathValue("rt"))
	})
	mux.HandleFunc("GET /api/v1/{rt}/tasks/{task}", func(w http.ResponseWriter, r *http.Request) {
		s.handleGetTask(w, r, r.PathValue("rt"), r.PathValue("task"))
	})
	mux.HandleFunc("DELETE /api/v1/{rt}/tasks/{task}", func(w http.ResponseWriter, r *http.Request) {
		s.handleCancelTask(w, r, r.PathValue("rt"), r.PathValue("task"))
	})
	mux.HandleFunc("POST /api/v1/{rt}/tasks/{task}/inputs/{input}", func(w http.ResponseWriter, r *http.Request) {
		s.handleInput(w, r, r.PathValue("rt"), r.PathValue("task"), r.PathValue("input"))
	})
	mux.HandleFunc("GET /api/v1/{rt}/sessions", func(w http.ResponseWriter, r *http.Request) {
		s.handleListSessions(w, r, r.PathValue("rt"))
	})
	mux.HandleFunc("PATCH /api/v1/{rt}/sessions/{sess}", func(w http.ResponseWriter, r *http.Request) {
		s.handlePatchSession(w, r, r.PathValue("rt"), r.PathValue("sess"))
	})
	mux.HandleFunc("GET /api/v1/{rt}/sessions/{sess}/messages", func(w http.ResponseWriter, r *http.Request) {
		s.handleSessionMessages(w, r, r.PathValue("rt"), r.PathValue("sess"))
	})
	// 附件上传：multipart 字段 "file"（可多个）→ data/uploads/<rt>/<sess>/
	mux.HandleFunc("POST /api/v1/{rt}/sessions/{sess}/attachments", func(w http.ResponseWriter, r *http.Request) {
		s.handleUploadAttachment(w, r, r.PathValue("rt"), r.PathValue("sess"))
	})
	mux.HandleFunc("GET /api/v1/{rt}/skills", func(w http.ResponseWriter, r *http.Request) {
		s.handleListSkills(w, r, r.PathValue("rt"))
	})
	mux.HandleFunc("POST /api/v1/{rt}/skills", func(w http.ResponseWriter, r *http.Request) {
		s.handleInstallSkill(w, r, r.PathValue("rt"))
	})
	mux.HandleFunc("PATCH /api/v1/{rt}/skills/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.handlePatchSkill(w, r, r.PathValue("rt"), r.PathValue("id"))
	})
	mux.HandleFunc("DELETE /api/v1/{rt}/skills/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.handleDeleteSkill(w, r, r.PathValue("rt"), r.PathValue("id"))
	})
	mux.HandleFunc("GET /api/v1/{rt}/mcp", func(w http.ResponseWriter, r *http.Request) {
		s.handleListMCP(w, r, r.PathValue("rt"))
	})
	mux.HandleFunc("POST /api/v1/{rt}/mcp", func(w http.ResponseWriter, r *http.Request) {
		s.handleCreateMCP(w, r, r.PathValue("rt"))
	})
	mux.HandleFunc("DELETE /api/v1/{rt}/mcp/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.handleDeleteMCP(w, r, r.PathValue("rt"), r.PathValue("id"))
	})

	// LLM gateway：Provider（1）→ Models（N）+ 全局激活指针
	if s.llm != nil {
		llmSvc := s.llm
		logged := func(h http.HandlerFunc) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				log.Printf("[llm] %s %s", r.Method, r.URL.Path)
				h(w, r)
			}
		}
		mux.HandleFunc("GET /api/v1/llm/providers", llmSvc.ListProviders)
		mux.HandleFunc("POST /api/v1/llm/providers", llmSvc.UpsertProvider)
		mux.HandleFunc("DELETE /api/v1/llm/providers/{id}", func(w http.ResponseWriter, r *http.Request) {
			llmSvc.DeleteProvider(w, r, r.PathValue("id"))
		})
		mux.HandleFunc("POST /api/v1/llm/providers/{id}/fetch-models", func(w http.ResponseWriter, r *http.Request) {
			llmSvc.FetchModels(w, r, r.PathValue("id"))
		})
		mux.HandleFunc("GET /api/v1/llm/active", llmSvc.GetActive)
		mux.HandleFunc("PUT /api/v1/llm/active", llmSvc.PutActive)
		mux.HandleFunc("POST /api/v1/llm/v1/chat/completions", logged(llmSvc.ProxyChat))
		// openclaw 的 openai-completions 客户端拼 baseUrl + "/chat/completions"（无 /v1）
		mux.HandleFunc("POST /api/v1/llm/chat/completions", logged(llmSvc.ProxyChat))
		// Anthropic Messages（claude-code：ANTHROPIC_BASE_URL=.../api/v1/llm → POST /v1/messages）
		mux.HandleFunc("POST /api/v1/llm/v1/messages", logged(llmSvc.ProxyAnthropic))
		mux.HandleFunc("POST /api/v1/llm/messages", logged(llmSvc.ProxyAnthropic))
	}

	return logAll(cors(s.auth(mux))) // cors 必须在 auth 外层：401 响应也要带 CORS 头
}

// auth：token 校验中间件。s.token 为空 = 关闭（本地开发默认）。
// 豁免 OPTIONS（CORS 预检）与 GET /api/v1/system/health（存活探测）。
func (s *Server) auth(next http.Handler) http.Handler {
	if s.token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions ||
			(r.Method == http.MethodGet && r.URL.Path == "/api/v1/system/health") {
			next.ServeHTTP(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.URL.Query().Get("access_token")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid token", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logAll 记录每个请求的 方法/路径/状态（排查客户端真实调用）。
func logAll(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		log.Printf("[http] %s %s → %d", r.Method, r.URL.Path, sw.status)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// cors: bridge 默认仅绑回环；宽松 CORS 允许浏览器直连 REST。
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Idempotency-Key, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string, details map[string]any) {
	writeJSON(w, code, map[string]any{
		"error": map[string]any{
			"code":       errCode,
			"message":    msg,
			"request_id": core.NewID("req_"),
			"details":    details,
		},
	})
}

// startTask 注册任务并启动适配器（REST 与 AG-UI 共用）。
// atts 非空时注入 prompt 尾部（仅给 adapter）；历史只存原始文本 + 附件元数据。
func (s *Server) startTask(runtimeID, threadID, prompt string, ad core.Adapter, atts ...core.Attachment) *core.Task {
	task := s.store.NewTask(runtimeID, threadID, prompt, ad, atts...)
	core.StartAdapterRun(task.TaskRunContext(), task, core.InjectAttachments(prompt, atts))
	return task
}

// ---- 全局端点 ----

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	runtimes := []map[string]any{}
	for id, rt := range s.adapters {
		st, ver := rt.Adapter.Health()
		runtimes = append(runtimes, map[string]any{"id": id, "status": st, "version": ver})
	}
	writeJSON(w, 200, map[string]any{
		"status":     "ok",
		"uptime_sec": int(time.Since(s.startedAt).Seconds()),
		"version":    "0.1.0",
		"runtimes":   runtimes,
	})
}

func (s *Server) handleRuntimes(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for id, rt := range s.adapters {
		st, ver := rt.Adapter.Health()
		out = append(out, map[string]any{
			"id": id, "name": rt.Adapter.Name(), "status": st, "version": ver,
			"last_seen": core.NowRFC3339(), "capabilities": rt.Adapter.Capabilities(),
		})
	}
	writeJSON(w, 200, map[string]any{"runtimes": out})
}

// ---- tasks ----

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request, rt string) {
	if _, ok := s.adapters[rt]; !ok {
		writeErr(w, 404, "runtime_not_found", "unknown runtime", nil)
		return
	}
	q := r.URL.Query()
	tasks := s.store.ListTasks(rt, q.Get("context_id"), q["status"])
	items := []map[string]any{}
	for _, t := range tasks {
		items = append(items, t.View())
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": nil})
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request, rt, taskID string) {
	t := s.store.GetTask(taskID)
	if t == nil || t.RuntimeID != rt {
		writeErr(w, 404, "task_not_found", "no such task", nil)
		return
	}
	writeJSON(w, 200, t.View())
}

func (s *Server) handlePostTask(w http.ResponseWriter, r *http.Request, rt string) {
	ar, ok := s.adapters[rt]
	if !ok {
		writeErr(w, 404, "runtime_not_found", "unknown runtime", nil)
		return
	}
	var body struct {
		Message     string             `json:"message"`
		ContextID   string             `json:"context_id"`
		Attachments []core.Attachment  `json:"attachments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Message == "" {
		writeErr(w, 400, "invalid_request", "message required", nil)
		return
	}
	threadID := body.ContextID
	if threadID == "" {
		threadID = core.NewID("ctx_")
	}
	task := s.startTask(rt, threadID, body.Message, ar.Adapter, body.Attachments...)
	writeJSON(w, 201, map[string]any{
		"task_id": task.TaskID, "context_id": threadID,
		"status": core.StateSubmitted, "created_at": task.CreatedAt,
	})
}

func (s *Server) handleInput(w http.ResponseWriter, r *http.Request, rt, taskID, inputID string) {
	t := s.store.GetTask(taskID)
	if t == nil || t.RuntimeID != rt {
		writeErr(w, 404, "task_not_found", "no such task", nil)
		return
	}
	var body struct {
		Action string         `json:"action"`
		Data   map[string]any `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid_request", "bad body", nil)
		return
	}
	summary := ""
	if in := t.PendingInput(); in != nil {
		summary = hitlSummary(in.Prompt)
	}
	if !core.ResolveInput(t, core.InputResponse{InputID: inputID, Action: body.Action, Data: body.Data}) {
		writeErr(w, 409, "input_not_pending", "input already answered or task not awaiting input", nil)
		return
	}
	if h, ok := t.BoundAdapter().(core.SessionHistory); ok {
		h.AppendMessage(t.ContextID, core.HistoryMsg{
			Role: "tool", TaskID: taskID, CreatedAt: core.NowRFC3339(),
			Content: core.MustJSON(map[string]any{
				"action": body.Action, "summary": summary, "data": body.Data,
			}),
		})
	}
	writeJSON(w, 200, t.View())
}

func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request, rt, taskID string) {
	t := s.store.GetTask(taskID)
	if t == nil || t.RuntimeID != rt {
		writeErr(w, 404, "task_not_found", "no such task", nil)
		return
	}
	if t.IsTerminal() {
		writeErr(w, 409, "task_already_terminal", "task already ended", nil)
		return
	}
	t.CancelRun()
	t.SetStatus(core.StateCanceled, "canceled by user")
	writeJSON(w, 200, t.View())
}

// ---- sessions ----

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request, rt string) {
	ar, ok := s.adapters[rt]
	if !ok {
		writeErr(w, 404, "runtime_not_found", "unknown runtime", nil)
		return
	}
	ss, ok := ar.Adapter.(core.SessionStore)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "capability_not_supported", "runtime has no sessions", nil)
		return
	}
	metas, err := ss.ListSessions()
	if err != nil {
		writeErr(w, 502, "runtime_error", err.Error(), nil)
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	items := []map[string]any{}
	for _, m := range metas {
		if q != "" && !strings.Contains(strings.ToLower(m.Title), q) &&
			!strings.Contains(strings.ToLower(m.ID), q) {
			continue
		}
		items = append(items, map[string]any{
			"session_id": m.ID, "runtime_id": rt, "title": m.Title,
			"task_count": m.TaskCount, "updated_at": m.UpdatedAt,
		})
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": nil})
}

func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request, rt, sessID string) {
	ar, ok := s.adapters[rt]
	if !ok {
		writeErr(w, 404, "runtime_not_found", "unknown runtime", nil)
		return
	}
	ss, ok := ar.Adapter.(core.SessionStore)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "capability_not_supported", "runtime has no sessions", nil)
		return
	}
	var body struct {
		Title *string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Title == nil {
		writeErr(w, 400, "invalid_request", "title required", nil)
		return
	}
	if err := ss.SetTitle(sessID, *body.Title); err != nil {
		writeErr(w, 502, "runtime_error", err.Error(), nil)
		return
	}
	for _, m := range mustList(ss) {
		if m.ID == sessID {
			writeJSON(w, 200, map[string]any{
				"session_id": m.ID, "runtime_id": rt, "title": m.Title,
				"task_count": m.TaskCount, "updated_at": m.UpdatedAt,
			})
			return
		}
	}
	writeErr(w, 404, "session_not_found", "no such session", nil)
}

func mustList(ss core.SessionStore) []core.SessionMeta {
	m, _ := ss.ListSessions()
	return m
}

func (s *Server) handleSessionMessages(w http.ResponseWriter, r *http.Request, rt, sessID string) {
	ar, ok := s.adapters[rt]
	if !ok {
		writeErr(w, 404, "runtime_not_found", "unknown runtime", nil)
		return
	}
	h, ok := ar.Adapter.(core.SessionHistory)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "capability_not_supported", "runtime has no history", nil)
		return
	}
	msgs, err := h.ListMessages(sessID)
	if err != nil {
		writeErr(w, 502, "runtime_error", err.Error(), nil)
		return
	}
	items := []map[string]any{}
	for _, m := range msgs {
		item := map[string]any{
			"role": m.Role, "content": m.Content, "thinking": m.Thinking,
			"task_id": m.TaskID, "created_at": m.CreatedAt,
		}
		if len(m.Attachments) > 0 {
			item["attachments"] = m.Attachments
		}
		items = append(items, item)
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": nil})
}

// ---- skills / mcp 管理 ----

func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request, rt string) {
	ar, ok := s.adapters[rt]
	if !ok {
		writeErr(w, 404, "runtime_not_found", "unknown runtime", nil)
		return
	}
	sp, ok := ar.Adapter.(core.SkillsProvider)
	if !ok || !ar.Adapter.Capabilities()["skills"] {
		writeErr(w, http.StatusNotImplemented, "capability_not_supported", "runtime has no skills module", nil)
		return
	}
	items, err := sp.ListSkills(ar.Workspace)
	if err != nil {
		writeErr(w, 502, "runtime_error", err.Error(), nil)
		return
	}
	if items == nil {
		items = []core.SkillItem{}
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": nil})
}

func (s *Server) handleInstallSkill(w http.ResponseWriter, r *http.Request, rt string) {
	ar, ok := s.adapters[rt]
	if !ok {
		writeErr(w, 404, "runtime_not_found", "unknown runtime", nil)
		return
	}
	prov, ok := ar.Adapter.(core.SkillsAdminProvider)
	if !ok {
		writeErr(w, 501, "capability_not_supported", "runtime has no skills admin", nil)
		return
	}
	var body struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" || body.Content == "" {
		writeErr(w, 400, "invalid_request", "name and content required", nil)
		return
	}
	item, err := prov.Admin().InstallSkill(ar.Workspace, body.Name, body.Content)
	if err != nil {
		writeErr(w, 409, "conflict", err.Error(), map[string]any{"reason": "skill_exists"})
		return
	}
	writeJSON(w, 201, item)
}

func (s *Server) handlePatchSkill(w http.ResponseWriter, r *http.Request, rt, skillID string) {
	ar, ok := s.adapters[rt]
	if !ok {
		writeErr(w, 404, "runtime_not_found", "unknown runtime", nil)
		return
	}
	prov, ok := ar.Adapter.(core.SkillsAdminProvider)
	if !ok {
		writeErr(w, 501, "capability_not_supported", "runtime has no skills admin", nil)
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err == nil && body.Enabled != nil {
		item, err := prov.Admin().SetSkillEnabled(ar.Workspace, skillID, *body.Enabled)
		if err != nil {
			writeErr(w, 404, "skill_not_found", err.Error(), nil)
			return
		}
		writeJSON(w, 200, item)
		return
	}
	writeErr(w, 400, "invalid_request", "body must contain enabled:boolean", nil)
}

func (s *Server) handleDeleteSkill(w http.ResponseWriter, r *http.Request, rt, skillID string) {
	ar, ok := s.adapters[rt]
	if !ok {
		writeErr(w, 404, "runtime_not_found", "unknown runtime", nil)
		return
	}
	prov, ok := ar.Adapter.(core.SkillsAdminProvider)
	if !ok {
		writeErr(w, 501, "capability_not_supported", "runtime has no skills admin", nil)
		return
	}
	if err := prov.Admin().RemoveSkill(ar.Workspace, skillID); err != nil {
		writeErr(w, 404, "skill_not_found", "no such skill (user-scope skills are read-only)", nil)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) handleListMCP(w http.ResponseWriter, r *http.Request, rt string) {
	ar, ok := s.adapters[rt]
	if !ok {
		writeErr(w, 404, "runtime_not_found", "unknown runtime", nil)
		return
	}
	mp, ok := ar.Adapter.(core.MCPProvider)
	if !ok || !ar.Adapter.Capabilities()["mcp"] {
		writeErr(w, http.StatusNotImplemented, "capability_not_supported", "runtime has no mcp module", nil)
		return
	}
	items, err := mp.ListMCP(ar.Workspace)
	if err != nil {
		writeErr(w, 502, "runtime_error", err.Error(), nil)
		return
	}
	if items == nil {
		items = []core.MCPServerItem{}
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": nil})
}

func (s *Server) handleCreateMCP(w http.ResponseWriter, r *http.Request, rt string) {
	ar, ok := s.adapters[rt]
	if !ok {
		writeErr(w, 404, "runtime_not_found", "unknown runtime", nil)
		return
	}
	prov, ok := ar.Adapter.(core.MCPAdminProvider)
	if !ok {
		writeErr(w, 501, "capability_not_supported", "runtime has no mcp admin", nil)
		return
	}
	var body struct {
		ID        string            `json:"id"`
		Name      string            `json:"name"`
		Transport string            `json:"transport"`
		Command   string            `json:"command"`
		Env       map[string]string `json:"env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (body.Name == "" && body.ID == "") || body.Command == "" {
		writeErr(w, 400, "invalid_request", "name and command required", nil)
		return
	}
	if body.Transport == "" {
		body.Transport = "stdio"
	}
	item, err := prov.MCPAdmin().UpsertMCP(ar.Workspace, core.MCPServerItem{
		ID: body.ID, Name: body.Name, Transport: body.Transport, Command: body.Command,
	}, body.Env)
	if err != nil {
		writeErr(w, 502, "runtime_error", err.Error(), nil)
		return
	}
	writeJSON(w, 201, item)
}

func (s *Server) handleDeleteMCP(w http.ResponseWriter, r *http.Request, rt, serverID string) {
	ar, ok := s.adapters[rt]
	if !ok {
		writeErr(w, 404, "runtime_not_found", "unknown runtime", nil)
		return
	}
	prov, ok := ar.Adapter.(core.MCPAdminProvider)
	if !ok {
		writeErr(w, 501, "capability_not_supported", "runtime has no mcp admin", nil)
		return
	}
	if err := prov.MCPAdmin().RemoveMCP(ar.Workspace, serverID); err != nil {
		writeErr(w, 404, "mcp_server_not_found", "no such server", nil)
		return
	}
	w.WriteHeader(204)
}
