// Anthropic Messages API ⇄ OpenAI Chat Completions 双向转换 + 代理。
//
// claude-code 等 Anthropic 客户端：
//
//	ANTHROPIC_BASE_URL=http://<bridge>/api/v1/llm
//	→ POST {base}/v1/messages
//
// 网关按激活指针改写 model，转发到上游 OpenAI 兼容接口，再把响应（含 SSE 流）
// 转回 Anthropic 事件格式。
package llmgw

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// ---- 入站：Anthropic Messages 请求 ----

type anthropicRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        json.RawMessage `json:"system,omitempty"`
	Messages      []anthropicMsg  `json:"messages"`
	Tools         []anthropicTool `json:"tools,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string 或 []block
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // tool_result 的结果
	IsError   bool            `json:"is_error,omitempty"`
	Source    json.RawMessage `json:"source,omitempty"` // image
}

// ---- 出站：Anthropic Messages 响应 ----

type anthropicResponse struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"`
	Role       string           `json:"role"`
	Content    []responseBlock  `json:"content"`
	Model      string           `json:"model"`
	StopReason string           `json:"stop_reason"`
	StopSeq    *string          `json:"stop_sequence"`
	Usage      anthropicUsage   `json:"usage"`
}

type responseBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ---- 转换：Anthropic → OpenAI ----

func systemText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

func parseBlocks(raw json.RawMessage) (string, []contentBlock) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return "", blocks
	}
	return "", nil
}

// anthropicToOpenAI 把 Messages 请求体转成 Chat Completions 请求体。
func anthropicToOpenAI(in *anthropicRequest, activeModel string) map[string]any {
	msgs := make([]map[string]any, 0, len(in.Messages)+1)
	if sys := systemText(in.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for _, m := range in.Messages {
		text, blocks := parseBlocks(m.Content)
		if len(blocks) == 0 {
			msgs = append(msgs, map[string]any{"role": m.Role, "content": text})
			continue
		}
		// assistant：text → content；tool_use → tool_calls
		// user：text / image → content parts；tool_result → tool 消息
		if m.Role == "assistant" {
			var parts []string
			var toolCalls []map[string]any
			for _, b := range blocks {
				switch b.Type {
				case "text":
					parts = append(parts, b.Text)
				case "tool_use":
					toolCalls = append(toolCalls, map[string]any{
						"id":   b.ID,
						"type": "function",
						"function": map[string]any{
							"name":      b.Name,
							"arguments": rawOrEmptyObject(b.Input),
						},
					})
				}
			}
			om := map[string]any{"role": "assistant", "content": strings.Join(parts, "")}
			if len(toolCalls) > 0 {
				om["tool_calls"] = toolCalls
			}
			msgs = append(msgs, om)
			continue
		}
		// user
		var toolResults []map[string]any
		var contentParts []map[string]any
		for _, b := range blocks {
			switch b.Type {
			case "text":
				contentParts = append(contentParts, map[string]any{"type": "text", "text": b.Text})
			case "image":
				// Anthropic base64 source → OpenAI data URL
				var src struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				}
				_ = json.Unmarshal(b.Source, &src)
				if src.Data != "" {
					contentParts = append(contentParts, map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "data:" + src.MediaType + ";base64," + src.Data},
					})
				}
			case "tool_result":
				toolResults = append(toolResults, map[string]any{
					"role":         "tool",
					"tool_call_id": b.ToolUseID,
					"content":      toolResultContent(b),
				})
			}
		}
		// tool_result 必须先于同轮其他 user 内容（OpenAI 要求独立 tool 消息）
		msgs = append(msgs, toolResults...)
		if len(contentParts) > 1 {
			msgs = append(msgs, map[string]any{"role": "user", "content": contentParts})
		} else if len(contentParts) == 1 {
			if contentParts[0]["type"] == "text" {
				msgs = append(msgs, map[string]any{"role": "user", "content": contentParts[0]["text"]})
			} else {
				msgs = append(msgs, map[string]any{"role": "user", "content": contentParts})
			}
		} else if len(toolResults) == 0 && text != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": text})
		}
	}

	out := map[string]any{
		"model":    activeModel,
		"messages": msgs,
	}
	if in.MaxTokens > 0 {
		out["max_tokens"] = in.MaxTokens
	} else {
		out["max_tokens"] = 4096
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if len(in.StopSequences) > 0 {
		out["stop"] = in.StopSequences
	}
	if len(in.Tools) > 0 {
		tools := make([]map[string]any, 0, len(in.Tools))
		for _, t := range in.Tools {
			params := json.RawMessage(`{"type":"object"}`)
			if len(t.InputSchema) > 0 {
				params = t.InputSchema
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  params,
				},
			})
		}
		out["tools"] = tools
		// 默认 auto；claude 侧未显式传 tool_choice 时保持 auto
		out["tool_choice"] = "auto"
	}
	if in.Stream {
		out["stream"] = true
	}
	return out
}

func rawOrEmptyObject(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	return string(raw)
}

// toolResultContent：tool_result.content 可为 string 或 text block 数组。
func toolResultContent(b contentBlock) string {
	if len(b.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(b.Content, &blocks); err == nil {
		var out strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" {
				out.WriteString(blk.Text)
			}
		}
		return out.String()
	}
	return string(b.Content)
}

// ---- 出站转换 ----

func finishToStopReason(fin string) string {
	switch fin {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length", "max_tokens":
		return "max_tokens"
	case "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}

type openaiResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Index        int `json:"index"`
		Message      struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Model string `json:"model"`
}

func openAIToAnthropic(raw []byte, requestedModel string) (*anthropicResponse, int, error) {
	var or openaiResponse
	if err := json.Unmarshal(raw, &or); err != nil {
		return nil, 0, err
	}
	if len(or.Choices) == 0 {
		return nil, 0, fmt.Errorf("upstream returned no choices")
	}
	ch := or.Choices[0]
	var content []responseBlock
	if ch.Message.Content != "" {
		content = append(content, responseBlock{Type: "text", Text: ch.Message.Content})
	}
	for _, tc := range ch.Message.ToolCalls {
		input := json.RawMessage(tc.Function.Arguments)
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		} else if !json.Valid(input) {
			// 尽力修复：空 arguments
			input = json.RawMessage(`{}`)
		}
		content = append(content, responseBlock{
			Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input,
		})
	}
	if content == nil {
		content = []responseBlock{}
	}
	model := or.Model
	if model == "" {
		model = requestedModel
	}
	resp := &anthropicResponse{
		ID:         or.ID,
		Type:       "message",
		Role:       "assistant",
		Content:    content,
		Model:      model,
		StopReason: finishToStopReason(ch.FinishReason),
		StopSeq:    nil,
		Usage: anthropicUsage{
			InputTokens:  or.Usage.PromptTokens,
			OutputTokens: or.Usage.CompletionTokens,
		},
	}
	if resp.ID == "" {
		resp.ID = "msg_upstream"
	}
	return resp, 0, nil
}

// ---- 代理入口 ----

// ProxyAnthropic 处理 POST /api/v1/llm[/v1]/messages。
func (s *Service) ProxyAnthropic(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeAnthropicErr(w, 400, "invalid_request", "read body failed")
		return
	}
	var in anthropicRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeAnthropicErr(w, 400, "invalid_request", "bad json: "+err.Error())
		return
	}

	active, err := s.store.GetActive()
	if err != nil || active.ProviderID == "" {
		writeAnthropicErr(w, 409, "no_active_model", "页面上先选择当前生效的模型")
		return
	}
	prov, err := s.store.GetProvider(active.ProviderID)
	if err != nil || prov == nil || !prov.Enabled {
		writeAnthropicErr(w, 502, "provider_unavailable", "active provider missing or disabled")
		return
	}

	log.Printf("[llm] anthropic requested_model=%q → active %s/%s stream=%v",
		in.Model, active.ProviderID, active.ModelID, in.Stream)

	oai := anthropicToOpenAI(&in, active.ModelID)
	fwd, _ := json.Marshal(oai)

	upURL := prov.BaseURL + "/v1/chat/completions"
	if strings.HasSuffix(prov.BaseURL, "/v1") {
		upURL = prov.BaseURL + "/chat/completions"
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upURL, bytes.NewReader(fwd))
	if err != nil {
		writeAnthropicErr(w, 500, "internal_error", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if prov.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+prov.APIKey)
	}
	if in.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := s.hc.Do(req)
	if err != nil {
		writeAnthropicErr(w, 502, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()

	if in.Stream {
		s.streamOpenAIToAnthropic(w, resp, active.ModelID)
		return
	}

	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		// 透传上游错误（尽量包一层 anthropic error 形状）
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(out)
		return
	}
	ar, _, err := openAIToAnthropic(out, active.ModelID)
	if err != nil {
		writeAnthropicErr(w, 502, "upstream_error", "bad upstream response: "+err.Error())
		return
	}
	writeJSON(w, 200, ar)
}

// streamOpenAIToAnthropic 把上游 OpenAI SSE 流实时翻译成 Anthropic 消息事件流。
func (s *Service) streamOpenAIToAnthropic(w http.ResponseWriter, resp *http.Response, model string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	flush := func() { if flusher != nil { flusher.Flush() } }

	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	send := func(v any) {
		b, _ := json.Marshal(v)
		typ, _ := v.(map[string]any)["type"].(string)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, b)
		flush()
	}
	// message_start
	send(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant",
			"content": []any{}, "model": model,
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})

	var (
		blockOpen      bool
		blockIndex     int
		blockKind      string // text | tool_use
		textOpen       bool
		stopReason     = "end_turn"
		promptTokens   int
		completionTok int
		closeBlock     func()
	)
	closeBlock = func() {
		if !blockOpen {
			return
		}
		send(map[string]any{"type": "content_block_stop", "index": blockIndex})
		blockIndex++
		blockOpen = false
		textOpen = false
	}
	openText := func() {
		if blockOpen {
			closeBlock()
		}
		blockKind = "text"
		blockOpen = true
		textOpen = true
		send(map[string]any{
			"type": "content_block_start",
			"index": blockIndex,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
	}
	openTool := func(id, name string) {
		if blockOpen {
			closeBlock()
		}
		blockKind = "tool_use"
		blockOpen = true
		send(map[string]any{
			"type": "content_block_start",
			"index": blockIndex,
			"content_block": map[string]any{
				"type": "tool_use", "id": id, "name": name, "input": map[string]any{},
			},
		})
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Role      string `json:"role"`
					Content   string `json:"content"`
					ToolCalls []struct {
						ID       string `json:"id"`
						Type     string `json:"type"`
						Index    int    `json:"index"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			promptTokens = chunk.Usage.PromptTokens
			completionTok = chunk.Usage.CompletionTokens
		}
		for _, c := range chunk.Choices {
			if c.FinishReason != "" {
				stopReason = finishToStopReason(c.FinishReason)
			}
			d := c.Delta
			if d.Content != "" {
				if !textOpen {
					openText()
				}
				send(map[string]any{
					"type": "content_block_delta",
					"index": blockIndex,
					"delta": map[string]any{"type": "text_delta", "text": d.Content},
				})
			}
			for _, tc := range d.ToolCalls {
				if tc.Function.Name != "" {
					openTool(tc.ID, tc.Function.Name)
				}
				if tc.Function.Arguments != "" && blockKind == "tool_use" {
					send(map[string]any{
						"type": "content_block_delta",
						"index": blockIndex,
						"delta": map[string]any{
							"type": "input_json_delta",
							"partial_json": tc.Function.Arguments,
						},
					})
				}
			}
		}
	}
	if blockOpen {
		closeBlock()
	}
	send(map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{"output_tokens": completionTok},
	})
	send(map[string]any{"type": "message_stop"})
	_ = promptTokens
}

func writeAnthropicErr(w http.ResponseWriter, code int, errType, msg string) {
	writeJSON(w, code, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	})
}
