package claude

// claude_mcp.go: 工作区 .mcp.json 的读取与读改写管理（密钥只写不读）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"xagent-bridge/internal/core"
)

// ---- mcp admin: read-modify-write workspace .mcp.json ----

type mcpAdmin struct{ c *Adapter }

func (c *Adapter) MCPAdmin() core.MCPAdmin { return mcpAdmin{c} }

func (a mcpAdmin) read(workspace string) (map[string]json.RawMessage, error) {
	path := filepath.Join(workspace, ".mcp.json")
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]json.RawMessage{}, nil
		}
		return nil, err
	}
	var cfg struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("bad .mcp.json: %w", err)
	}
	if cfg.MCPServers == nil {
		cfg.MCPServers = map[string]json.RawMessage{}
	}
	return cfg.MCPServers, nil
}

func (a mcpAdmin) write(workspace string, servers map[string]json.RawMessage) error {
	out, err := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(workspace, ".mcp.json"), append(out, '\n'), 0o644)
}

func (a mcpAdmin) UpsertMCP(workspace string, item core.MCPServerItem, env map[string]string) (core.MCPServerItem, error) {
	id := item.ID
	if id == "" {
		id = slugify(item.Name)
	}
	if id == "" {
		return core.MCPServerItem{}, fmt.Errorf("invalid name")
	}
	entry := map[string]any{}
	if item.Transport == "http" || item.Transport == "sse" {
		entry["type"] = item.Transport
		entry["url"] = item.Command
	} else {
		parts := strings.Fields(item.Command)
		if len(parts) == 0 {
			return core.MCPServerItem{}, fmt.Errorf("command required")
		}
		entry["command"] = parts[0]
		if len(parts) > 1 {
			entry["args"] = parts[1:]
		}
	}
	if len(env) > 0 {
		entry["env"] = env
	}
	servers, err := a.read(workspace)
	if err != nil {
		return core.MCPServerItem{}, err
	}
	raw, _ := json.Marshal(entry)
	servers[id] = raw
	if err := a.write(workspace, servers); err != nil {
		return core.MCPServerItem{}, err
	}
	item.ID, item.Name, item.Enabled, item.Status = id, id, true, "unknown"
	return item, nil
}

func (a mcpAdmin) RemoveMCP(workspace, id string) error {
	servers, err := a.read(workspace)
	if err != nil {
		return err
	}
	if _, ok := servers[id]; !ok {
		return core.ErrSkillNotFound // reuse; handler maps by endpoint
	}
	delete(servers, id)
	return a.write(workspace, servers)
}

func (c *Adapter) ListMCP(workspace string) ([]core.MCPServerItem, error) {
	path := filepath.Join(workspace, ".mcp.json")
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
			Url     string            `json:"url"`
			Type    string            `json:"type"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("bad .mcp.json: %w", err)
	}
	out := []core.MCPServerItem{}
	for id, srv := range cfg.MCPServers {
		transport := "stdio"
		cmd := strings.TrimSpace(srv.Command + " " + strings.Join(srv.Args, " "))
		if srv.Url != "" {
			transport = srv.Type
			if transport == "" {
				transport = "http"
			}
			cmd = srv.Url
		}
		// secret hygiene: values masked (spec §8.2)
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
