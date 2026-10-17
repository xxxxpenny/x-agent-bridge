// Package sesscache: 适配器私有的会话缓存（thread→native handle、标题、消息索引）。
// agent 侧会话存储是权威源；此缓存仅为 bridge UI 的写透索引。
//
// 存储：SQLite（WAL）单库 data/sessions.db，runtime 命名空间列隔离。
// 写入为单条 INSERT/UPSERT（O(1)），替代旧 JSON 全量重写。
package sesscache

import (
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"

	"xagent-bridge/internal/core"

	_ "modernc.org/sqlite"
)

type Cache struct {
	ns string // runtime 命名空间（如 "acp"、"claude-code-legacy"）
	db *sql.DB
	mu sync.Mutex // 仅保护同名 db 句柄初始化外的低频元操作
}

// Open 打开（或初始化）dataDir/sessions.db；name 为 runtime 命名空间。
// 兼容旧 <name>-sessions.json：首次打开时自动迁移并改名 .bak。
func Open(dataDir, name string) *Cache {
	path := filepath.Join(dataDir, "sessions.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		log.Printf("[sesscache] open %s: %v", path, err)
		return &Cache{ns: name}
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		_, _ = db.Exec(pragma)
	}
	_, _ = db.Exec(`
CREATE TABLE IF NOT EXISTS sessions (
  runtime TEXT NOT NULL,
  thread_id TEXT NOT NULL,
  handle TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL DEFAULT '',
  task_count INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (runtime, thread_id)
);
CREATE TABLE IF NOT EXISTS messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  runtime TEXT NOT NULL,
  thread_id TEXT NOT NULL,
  role TEXT NOT NULL,
  content TEXT NOT NULL,
  thinking TEXT NOT NULL DEFAULT '',
  task_id TEXT NOT NULL DEFAULT '',
  attachments TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_rt_thread ON messages(runtime, thread_id, id);
`)
	// 老库补列（CREATE IF NOT EXISTS 不会给已存在的表加列）
	_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN attachments TEXT NOT NULL DEFAULT '[]'`)
	c := &Cache{ns: name, db: db}
	migrateFromJSON(dataDir, name, db)
	return c
}

// migrateFromJSON 一次性导入旧 JSON 缓存（存在时）。
func migrateFromJSON(dataDir, name string, db *sql.DB) {
	old := filepath.Join(dataDir, name+"-sessions.json")
	b, err := os.ReadFile(old)
	if err != nil {
		return
	}
	var ents map[string]struct {
		Title     string            `json:"title"`
		UpdatedAt string            `json:"updatedAt"`
		TaskCount int               `json:"taskCount"`
		Handle    string            `json:"handle"`
		Messages  []core.HistoryMsg `json:"messages"`
	}
	if json.Unmarshal(b, &ents) != nil {
		return
	}
	for id, e := range ents {
		_, _ = db.Exec(`INSERT OR IGNORE INTO sessions(runtime,thread_id,handle,title,task_count,updated_at)
			VALUES(?,?,?,?,?,?)`, name, id, e.Handle, e.Title, e.TaskCount, e.UpdatedAt)
		for _, m := range e.Messages {
			_, _ = db.Exec(`INSERT INTO messages(runtime,thread_id,role,content,thinking,task_id,created_at)
				VALUES(?,?,?,?,?,?,?)`, name, id, m.Role, m.Content, m.Thinking, m.TaskID, m.CreatedAt)
		}
	}
	_ = os.Rename(old, old+".bak")
	log.Printf("[sesscache] migrated %d sessions from %s", len(ents), filepath.Base(old))
}

func (c *Cache) ensure() bool { return c.db != nil }

// ---- core.SessionStore ----

func (c *Cache) EnsureSession(id, seedTitle string) error {
	if !c.ensure() {
		return nil
	}
	_, err := c.db.Exec(`INSERT INTO sessions(runtime,thread_id,title,task_count,updated_at)
		VALUES(?,?,?,1,?) ON CONFLICT(runtime,thread_id) DO UPDATE SET task_count=task_count+1, updated_at=excluded.updated_at`,
		c.ns, id, seedTitle, core.NowRFC3339())
	if err == nil && seedTitle != "" {
		// 仅当原标题为空时用 seed 覆盖
		_, _ = c.db.Exec(`UPDATE sessions SET title=? WHERE runtime=? AND thread_id=? AND (title='' OR title='New session')`,
			seedTitle, c.ns, id)
	}
	return err
}

func (c *Cache) ListSessions() ([]core.SessionMeta, error) {
	if !c.ensure() {
		return nil, nil
	}
	rows, err := c.db.Query(`SELECT thread_id,title,task_count,updated_at FROM sessions WHERE runtime=? ORDER BY updated_at DESC`, c.ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.SessionMeta{}
	for rows.Next() {
		var m core.SessionMeta
		if err := rows.Scan(&m.ID, &m.Title, &m.TaskCount, &m.UpdatedAt); err != nil {
			continue
		}
		if m.Title == "" {
			m.Title = "New session"
		}
		out = append(out, m)
	}
	return out, nil
}

func (c *Cache) SetTitle(id, title string) error {
	if !c.ensure() {
		return nil
	}
	_, err := c.db.Exec(`UPDATE sessions SET title=?, updated_at=? WHERE runtime=? AND thread_id=?`,
		title, core.NowRFC3339(), c.ns, id)
	return err
}

// ---- core.SessionHistory ----

func (c *Cache) ListMessages(id string) ([]core.HistoryMsg, error) {
	if !c.ensure() {
		return nil, nil
	}
	// 保留最近 500 条
	rows, err := c.db.Query(`SELECT role,content,thinking,task_id,attachments,created_at FROM (
		SELECT id,role,content,thinking,task_id,attachments,created_at FROM messages
		WHERE runtime=? AND thread_id=? ORDER BY id DESC LIMIT 500
	) ORDER BY id ASC`, c.ns, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.HistoryMsg{}
	for rows.Next() {
		var m core.HistoryMsg
		var attsJSON string
		if err := rows.Scan(&m.Role, &m.Content, &m.Thinking, &m.TaskID, &attsJSON, &m.CreatedAt); err != nil {
			continue
		}
		if attsJSON != "" && attsJSON != "[]" {
			_ = json.Unmarshal([]byte(attsJSON), &m.Attachments)
		}
		out = append(out, m)
	}
	return out, nil
}

func (c *Cache) AppendMessage(id string, m core.HistoryMsg) error {
	if !c.ensure() {
		return nil
	}
	attsJSON := "[]"
	if len(m.Attachments) > 0 {
		if b, err := json.Marshal(m.Attachments); err == nil {
			attsJSON = string(b)
		}
	}
	_, err := c.db.Exec(`INSERT INTO messages(runtime,thread_id,role,content,thinking,task_id,attachments,created_at)
		VALUES(?,?,?,?,?,?,?,?)`, c.ns, id, m.Role, m.Content, m.Thinking, m.TaskID, attsJSON, m.CreatedAt)
	if err == nil {
		_, _ = c.db.Exec(`UPDATE sessions SET updated_at=? WHERE runtime=? AND thread_id=?`,
			core.NowRFC3339(), c.ns, id)
	}
	return err
}

// ---- handle 映射 ----

func (c *Cache) Handle(thread string) string {
	if !c.ensure() {
		return ""
	}
	var h string
	_ = c.db.QueryRow(`SELECT handle FROM sessions WHERE runtime=? AND thread_id=?`, c.ns, thread).Scan(&h)
	return h
}

func (c *Cache) SetHandle(thread, handle string) {
	if !c.ensure() {
		return
	}
	_, _ = c.db.Exec(`INSERT INTO sessions(runtime,thread_id,handle,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(runtime,thread_id) DO UPDATE SET handle=excluded.handle, updated_at=excluded.updated_at`,
		c.ns, thread, handle, core.NowRFC3339())
}
