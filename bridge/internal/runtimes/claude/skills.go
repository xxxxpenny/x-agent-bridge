package claude

// claude_skills.go: SKILL.md 目录扫描 + project 作用域的安装/启停/删除。
// 启停通过重命名 SKILL.md ↔ SKILL.md.disabled 强制生效。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"xagent-bridge/internal/core"
)

// ---- skills: Claude Code loads SKILL.md dirs from project + user scope ----

func (c *Adapter) ListSkills(workspace string) ([]core.SkillItem, error) {
	home, _ := os.UserHomeDir()
	roots := []string{
		filepath.Join(workspace, ".claude", "skills"),
		filepath.Join(home, ".claude", "skills"),
	}
	seen := map[string]bool{}
	var out []core.SkillItem
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			on := filepath.Join(root, e.Name(), "SKILL.md")
			off := filepath.Join(root, e.Name(), "SKILL.md.disabled")
			body, err := os.ReadFile(on)
			enabled := true
			if err != nil {
				body, err = os.ReadFile(off)
				enabled = false
				if err != nil {
					continue
				}
			}
			if seen[e.Name()] {
				continue
			}
			seen[e.Name()] = true
			name, desc := core.ParseSkillFrontmatter(string(body))
			if name == "" {
				name = e.Name()
			}
			src := "project"
			if !strings.HasPrefix(root, workspace) {
				src = "user"
			}
			out = append(out, core.SkillItem{
				ID: e.Name(), Name: name, Description: desc,
				Enabled: enabled, Source: src,
			})
		}
	}
	return out, nil
}

// ---- skills admin (project scope): real filesystem enforcement ----
// disable = rename SKILL.md -> SKILL.md.disabled (Claude only loads SKILL.md);
// enable renames back. Mutations only allowed for workspace-scoped skills.

type skillsAdmin struct{ c *Adapter }

func (c *Adapter) Admin() core.SkillsAdmin { return skillsAdmin{c} }

func slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '/' || r == '.':
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func skillDir(workspace, id string) string {
	return filepath.Join(workspace, ".claude", "skills", id)
}

func (a skillsAdmin) InstallSkill(workspace, name, content string) (core.SkillItem, error) {
	id := slugify(name)
	if id == "" {
		return core.SkillItem{}, fmt.Errorf("invalid name")
	}
	dir := skillDir(workspace, id)
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err == nil {
		return core.SkillItem{}, core.ErrSkillExists
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return core.SkillItem{}, err
	}
	body := content
	if !strings.HasPrefix(strings.TrimSpace(content), "---") {
		body = fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s", name, name, content)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		return core.SkillItem{}, err
	}
	return core.SkillItem{ID: id, Name: name, Description: "", Enabled: true, Source: "project"}, nil
}

func (a skillsAdmin) SetSkillEnabled(workspace, id string, enabled bool) (core.SkillItem, error) {
	dir := skillDir(workspace, id)
	on := filepath.Join(dir, "SKILL.md")
	off := filepath.Join(dir, "SKILL.md.disabled")
	if enabled {
		if err := os.Rename(off, on); err != nil && !os.IsNotExist(err) {
			return core.SkillItem{}, err
		}
	} else {
		if err := os.Rename(on, off); err != nil {
			return core.SkillItem{}, err
		}
	}
	items, _ := a.c.ListSkills(workspace)
	for _, it := range items {
		if it.ID == id {
			it.Enabled = enabled
			return it, nil
		}
	}
	return core.SkillItem{ID: id, Name: id, Enabled: enabled, Source: "project"}, nil
}

func (a skillsAdmin) RemoveSkill(workspace, id string) error {
	dir := skillDir(workspace, id)
	if _, err := os.Stat(dir); err != nil {
		return core.ErrSkillNotFound
	}
	return os.RemoveAll(dir)
}
