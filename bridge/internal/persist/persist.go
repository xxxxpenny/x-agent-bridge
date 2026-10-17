// Package persist: LLM 网关配置的 SQLite 存储。
//
// v2 重构后，会话/历史/标题由各 runtime adapter 自管（ACP 回放为权威源，
// adapter 内部缓存为 UI 索引），core 不再持有全局持久化。
package persist

import (
	"database/sql"
	"encoding/json"

	"xagent-bridge/internal/llmgw"

	_ "modernc.org/sqlite"
)

type DB struct{ *sql.DB }

func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			// pragma 失败不致命
			_ = err
		}
	}
	schema := `
CREATE TABLE IF NOT EXISTS llm_providers (
  id TEXT PRIMARY KEY,
  base_url TEXT NOT NULL,
  api_key TEXT NOT NULL DEFAULT '',
  models TEXT NOT NULL DEFAULT '[]',
  enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS llm_active (
  key TEXT NOT NULL PRIMARY KEY CHECK (key = 'active'),
  provider_id TEXT NOT NULL,
  model_id TEXT NOT NULL
);
`
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &DB{db}, nil
}

// ---- llm providers + active pointer（llmgw.Store 实现） ----

func (d *DB) ListProviders() ([]llmgw.Provider, error) {
	rows, err := d.Query(`SELECT id,base_url,api_key,models,enabled FROM llm_providers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []llmgw.Provider{}
	for rows.Next() {
		var p llmgw.Provider
		var models string
		var enabled int
		rows.Scan(&p.ID, &p.BaseURL, &p.APIKey, &models, &enabled)
		_ = json.Unmarshal([]byte(models), &p.Models)
		p.Enabled = enabled == 1
		out = append(out, p)
	}
	return out, nil
}

func (d *DB) GetProvider(id string) (*llmgw.Provider, error) {
	var p llmgw.Provider
	var models string
	var enabled int
	err := d.QueryRow(`SELECT id,base_url,api_key,models,enabled FROM llm_providers WHERE id=?`, id).
		Scan(&p.ID, &p.BaseURL, &p.APIKey, &models, &enabled)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(models), &p.Models)
	p.Enabled = enabled == 1
	return &p, nil
}

func (d *DB) UpsertProvider(p llmgw.Provider) error {
	models, _ := json.Marshal(p.Models)
	en := 0
	if p.Enabled {
		en = 1
	}
	_, err := d.Exec(`INSERT INTO llm_providers(id,base_url,api_key,models,enabled)
		VALUES(?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET base_url=excluded.base_url, api_key=excluded.api_key,
		models=excluded.models, enabled=excluded.enabled`,
		p.ID, p.BaseURL, p.APIKey, string(models), en)
	return err
}

func (d *DB) DeleteProvider(id string) error {
	_, err := d.Exec(`DELETE FROM llm_providers WHERE id=?`, id)
	if err == nil {
		_, _ = d.Exec(`DELETE FROM llm_active WHERE provider_id=?`, id)
	}
	return err
}

func (d *DB) GetActive() (llmgw.Active, error) {
	var a llmgw.Active
	err := d.QueryRow(`SELECT provider_id,model_id FROM llm_active WHERE key='active'`).Scan(&a.ProviderID, &a.ModelID)
	return a, err
}

func (d *DB) SetActive(a llmgw.Active) error {
	_, err := d.Exec(`INSERT INTO llm_active(key,provider_id,model_id) VALUES('active',?,?)
		ON CONFLICT(key) DO UPDATE SET provider_id=excluded.provider_id, model_id=excluded.model_id`,
		a.ProviderID, a.ModelID)
	return err
}
