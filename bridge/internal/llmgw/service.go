// Package llmgw: OpenAI 兼容的 LLM 网关。
//
// 数据模型（用户设计）：
//   Provider（1）── Models（N）
//   全局"当前激活"指针 = (provider, model)
// agent 侧写死本地 URL + 固定模型名（如 xagent/mimo），
// 代理一律按激活指针转发并改写 model —— 页面切换立即生效，agent 零改动。
package llmgw

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type ModelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type Provider struct {
	ID       string      `json:"id"`
	BaseURL  string      `json:"base_url"`
	APIKey   string      `json:"-"`
	HasKey   bool        `json:"has_key"`
	Models   []ModelInfo `json:"models"`
	Enabled  bool        `json:"enabled"`
	Upstream string      `json:"upstream,omitempty"` // 上游类型，仅展示
}

type Active struct {
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
}

type Store interface {
	ListProviders() ([]Provider, error)
	GetProvider(id string) (*Provider, error)
	UpsertProvider(p Provider) error
	DeleteProvider(id string) error
	GetActive() (Active, error)
	SetActive(a Active) error
}

type Service struct {
	store Store
	mu    sync.RWMutex
	hc    *http.Client
}

func NewService(store Store) *Service {
	return &Service{store: store, hc: &http.Client{Timeout: 0}}
}

// ---- provider CRUD ----

func (s *Service) ListProviders(w http.ResponseWriter, r *http.Request) {
	provs, err := s.store.ListProviders()
	if err != nil {
		writeErr(w, 500, "internal_error", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": providerViews(provs)})
}

func providerViews(provs []Provider) []map[string]any {
	out := make([]map[string]any, 0, len(provs))
	for _, p := range provs {
		key := ""
		if p.APIKey != "" {
			key = "***"
		}
		out = append(out, map[string]any{
			"id": p.ID, "base_url": p.BaseURL, "api_key": key,
			"models": p.Models, "enabled": p.Enabled,
		})
	}
	return out
}

func (s *Service) UpsertProvider(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string `json:"id"`
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" || body.BaseURL == "" {
		writeErr(w, 400, "invalid_request", "id and base_url required")
		return
	}
	// 已有 provider 且未传 key → 保留
	existing, _ := s.store.GetProvider(body.ID)
	key := body.APIKey
	var models []ModelInfo
	enabled := true
	if existing != nil {
		if body.APIKey == "" {
			key = existing.APIKey
		}
		models = existing.Models
		if body.Enabled != nil {
			enabled = *body.Enabled
		} else {
			enabled = existing.Enabled
		}
	}
	p := Provider{
		ID: body.ID, BaseURL: strings.TrimRight(body.BaseURL, "/"),
		APIKey: key, Models: models, Enabled: enabled,
	}
	if err := s.store.UpsertProvider(p); err != nil {
		writeErr(w, 500, "internal_error", err.Error())
		return
	}
	writeJSON(w, 200, providerViews([]Provider{p})[0])
}

func (s *Service) DeleteProvider(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.store.DeleteProvider(id); err != nil {
		writeErr(w, 500, "internal_error", err.Error())
		return
	}
	w.WriteHeader(204)
}

// FetchModels 从上游 GET {base}/v1/models 拉取模型列表。
func (s *Service) FetchModels(w http.ResponseWriter, r *http.Request, id string) {
	p, err := s.store.GetProvider(id)
	if err != nil || p == nil {
		writeErr(w, 404, "not_found", "no such provider")
		return
	}
	url := p.BaseURL + "/v1/models"
	if strings.HasSuffix(p.BaseURL, "/v1") {
		url = p.BaseURL + "/models"
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		writeErr(w, 500, "internal_error", err.Error())
		return
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		writeErr(w, 502, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		writeErr(w, 502, "upstream_error", "bad upstream /models response: "+err.Error())
		return
	}
	models := make([]ModelInfo, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID != "" {
			models = append(models, ModelInfo{ID: m.ID, Name: m.Name})
		}
	}
	p.Models = models
	if err := s.store.UpsertProvider(*p); err != nil {
		writeErr(w, 500, "internal_error", err.Error())
		return
	}
	writeJSON(w, 200, providerViews([]Provider{*p})[0])
}

// ---- active pointer ----

func (s *Service) GetActive(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.GetActive()
	if err != nil || a.ProviderID == "" {
		writeJSON(w, 200, map[string]any{"provider_id": "", "model_id": ""})
		return
	}
	writeJSON(w, 200, a)
}

func (s *Service) PutActive(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProviderID string `json:"provider_id"`
		ModelID    string `json:"model_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ProviderID == "" || body.ModelID == "" {
		writeErr(w, 400, "invalid_request", "provider_id and model_id required")
		return
	}
	p, err := s.store.GetProvider(body.ProviderID)
	if err != nil || p == nil {
		writeErr(w, 404, "not_found", "no such provider")
		return
	}
	found := false
	for _, m := range p.Models {
		if m.ID == body.ModelID {
			found = true
			break
		}
	}
	if !found {
		writeErr(w, 400, "invalid_request", "provider has no model "+body.ModelID)
		return
	}
	if err := s.store.SetActive(Active{ProviderID: body.ProviderID, ModelID: body.ModelID}); err != nil {
		writeErr(w, 500, "internal_error", err.Error())
		return
	}
	log.Printf("[llm] active → %s/%s", body.ProviderID, body.ModelID)
	writeJSON(w, 200, body)
}

// ---- 代理 ----

func (s *Service) ProxyChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeErr(w, 400, "invalid_request", "read body failed")
		return
	}
	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		writeErr(w, 400, "invalid_request", "bad json")
		return
	}

	active, err := s.store.GetActive()
	if err != nil || active.ProviderID == "" {
		writeErr(w, 409, "no_active_model", "页面上先选择当前生效的模型")
		return
	}
	prov, err := s.store.GetProvider(active.ProviderID)
	if err != nil || prov == nil || !prov.Enabled {
		writeErr(w, 502, "provider_unavailable", "active provider missing or disabled")
		return
	}
	log.Printf("[llm] chat requested_model=%q → active %s/%s stream=%v path=%s",
		probe.Model, active.ProviderID, active.ModelID, probe.Stream, r.URL.Path)

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeErr(w, 400, "invalid_request", "bad json")
		return
	}
	payload["model"] = active.ModelID // 按激活指针改写
	fwd, _ := json.Marshal(payload)

	upURL := prov.BaseURL + "/v1/chat/completions"
	if strings.HasSuffix(prov.BaseURL, "/v1") {
		upURL = prov.BaseURL + "/chat/completions"
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upURL, bytes.NewReader(fwd))
	if err != nil {
		writeErr(w, 500, "internal_error", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if prov.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+prov.APIKey)
	}
	if a := r.Header.Get("Accept"); a != "" {
		req.Header.Set("Accept", a)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		writeErr(w, 502, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		w.WriteHeader(resp.StatusCode)
		flusher, _ := w.(http.Flusher)
		buf := make([]byte, 32<<10)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				if flusher != nil {
					flusher.Flush()
				}
			}
			if err != nil {
				return
			}
		}
	}
	out, _ := io.ReadAll(resp.Body)
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]any{
		"error": map[string]any{"code": errCode, "message": msg, "request_id": fmt.Sprintf("llm_%d", time.Now().UnixNano())},
	})
}
