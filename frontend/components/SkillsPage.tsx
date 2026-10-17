"use client";

import { useState } from "react";
import { api, type Skill } from "../lib/types";

export function SkillsPage({
  agent, skills, onChanged,
}: { agent: string; skills: Skill[] | null; onChanged: () => Promise<void> }) {
  const [installing, setInstalling] = useState(false);
  const [form, setForm] = useState({ name: "", content: "" });
  const [err, setErr] = useState("");

  const submit = async () => {
    setErr("");
    try {
      await api(`/api/v1/${agent}/skills`, { method: "POST", body: JSON.stringify(form) });
      setForm({ name: "", content: "" });
      setInstalling(false);
      await onChanged();
    } catch (e: any) { setErr(e.message); }
  };

  const toggle = async (sk: Skill) => {
    try {
      await api(`/api/v1/${agent}/skills/${sk.id}`, {
        method: "PATCH", body: JSON.stringify({ enabled: !sk.enabled }),
      });
      await onChanged();
    } catch (e: any) { setErr(e.message); }
  };

  const remove = async (sk: Skill) => {
    if (!window.confirm(`删除技能 ${sk.name}？（不可恢复）`)) return;
    try {
      await api(`/api/v1/${agent}/skills/${sk.id}`, { method: "DELETE" });
      await onChanged();
    } catch (e: any) { setErr(e.message); }
  };

  if (skills === null) {
    return <div className="page"><div className="page-empty">该 runtime 不支持 Skills 模块</div></div>;
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>Skills</h1>
          <p>该 runtime 加载的技能（扫描 skills 目录）。禁用会将 SKILL.md 重命名，对新任务生效。</p>
        </div>
        <button className="btn-primary" data-testid="install-skill-btn"
          onClick={() => setInstalling(v => !v)}>
          {installing ? "取消" : "＋ 安装 Skill"}
        </button>
      </div>

      {installing && (
        <div className="form-card">
          <div className="form-row">
            <label>名称<input value={form.name} placeholder="如：release-notes"
              onChange={e => setForm({ ...form, name: e.target.value })} /></label>
          </div>
          <div className="form-row">
            <label>内容（SKILL.md 正文）<textarea rows={6}
              placeholder="技能说明…"
              value={form.content} onChange={e => setForm({ ...form, content: e.target.value })} /></label>
          </div>
          {err && <div className="form-err">{err}</div>}
          <div className="form-actions">
            <button className="btn-primary" data-testid="install-skill-submit"
              onClick={submit} disabled={!form.name || !form.content}>安装</button>
          </div>
        </div>
      )}

      <div className="table">
        <div className="thead">
          <div className="th c-name">名称</div>
          <div className="th c-desc">描述</div>
          <div className="th c-src">来源</div>
          <div className="th c-status">状态</div>
          <div className="th c-ops">操作</div>
        </div>
        {skills.map(sk => (
          <div className="trow" key={sk.id} data-testid={`skill-${sk.id}`}>
            <div className="td c-name">
              <span className={`status-dot ${sk.enabled ? "dot-green" : "dot-gray"}`} />
              <div className="row-stack">
                <span className="row-title">{sk.name}</span>
                <span className="row-sub">{sk.id}</span>
              </div>
            </div>
            <div className="td c-desc">{sk.description || "—"}</div>
            <div className="td c-src"><span className="tag">{sk.source}</span></div>
            <div className="td c-status">
              <span className={`pill ${sk.enabled ? "pill-green" : "pill-gray"}`}>
                {sk.enabled ? "启用" : "禁用"}
              </span>
            </div>
            <div className="td c-ops">
              <button className="btn-ghost" onClick={() => toggle(sk)}>
                {sk.enabled ? "禁用" : "启用"}
              </button>
              <button className="btn-ghost btn-danger" onClick={() => remove(sk)}>删除</button>
            </div>
          </div>
        ))}
        {skills.length === 0 && <div className="table-empty">暂无技能 — 点右上角安装第一个</div>}
      </div>
      {err && !installing && <div className="form-err">{err}</div>}
    </div>
  );
}
