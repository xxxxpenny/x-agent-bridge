"use client";

import { useState } from "react";
import { api, type Mcp } from "../lib/types";

export function McpPage({
  agent, servers, onChanged,
}: { agent: string; servers: Mcp[] | null; onChanged: () => Promise<void> }) {
  const [adding, setAdding] = useState(false);
  const [form, setForm] = useState({ name: "", command: "", transport: "stdio", env: "" });
  const [err, setErr] = useState("");

  const parseEnv = () => {
    const env: Record<string, string> = {};
    for (const line of form.env.split("\n")) {
      const i = line.indexOf("=");
      if (i > 0) env[line.slice(0, i).trim()] = line.slice(i + 1).trim();
    }
    return env;
  };

  const submit = async () => {
    setErr("");
    try {
      await api(`/api/v1/${agent}/mcp`, {
        method: "POST",
        body: JSON.stringify({ name: form.name, command: form.command, transport: form.transport, env: parseEnv() }),
      });
      setForm({ name: "", command: "", transport: "stdio", env: "" });
      setAdding(false);
      await onChanged();
    } catch (e: any) { setErr(e.message); }
  };

  const remove = async (m: Mcp) => {
    if (!window.confirm(`移除 ${m.name}？`)) return;
    try {
      await api(`/api/v1/${agent}/mcp/${m.id}`, { method: "DELETE" });
      await onChanged();
    } catch (e: any) { setErr(e.message); }
  };

  if (servers === null) {
    return <div className="page"><div className="page-empty">该 runtime 不支持 MCP 模块</div></div>;
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>MCP Servers</h1>
          <p>该 runtime 的 MCP 服务。密钥值只写不读，列表中一律脱敏为 <code>***</code>。</p>
        </div>
        <button className="btn-primary" data-testid="add-mcp-btn"
          onClick={() => setAdding(v => !v)}>
          {adding ? "取消" : "＋ 添加 MCP"}
        </button>
      </div>

      {adding && (
        <div className="form-card">
          <div className="form-grid">
            <label>名称<input value={form.name} placeholder="如：github"
              onChange={e => setForm({ ...form, name: e.target.value })} /></label>
            <label>类型
              <select value={form.transport}
                onChange={e => setForm({ ...form, transport: e.target.value })}>
                <option value="stdio">stdio</option>
                <option value="http">http</option>
                <option value="sse">sse</option>
              </select>
            </label>
          </div>
          <div className="form-row">
            <label>{form.transport === "stdio" ? "命令（含参数）" : "URL"}
              <input value={form.command}
                placeholder={form.transport === "stdio" ? "npx -y @modelcontextprotocol/server-xxx" : "https://…"}
                onChange={e => setForm({ ...form, command: e.target.value })} />
            </label>
          </div>
          <div className="form-row">
            <label>环境变量（每行 KEY=VALUE，留空则无）
              <textarea rows={3} placeholder="GITHUB_TOKEN=ghp_..."
                value={form.env} onChange={e => setForm({ ...form, env: e.target.value })} />
            </label>
          </div>
          {err && <div className="form-err">{err}</div>}
          <div className="form-actions">
            <button className="btn-primary" data-testid="add-mcp-submit"
              onClick={submit} disabled={!form.name || !form.command}>添加</button>
          </div>
        </div>
      )}

      <div className="table">
        <div className="thead">
          <div className="th c-mname">名称</div>
          <div className="th c-mtransport">类型</div>
          <div className="th c-mcmd">命令 / URL</div>
          <div className="th c-menv">密钥</div>
          <div className="th c-ops">操作</div>
        </div>
        {servers.map(m => (
          <div className="trow" key={m.id} data-testid={`mcp-${m.id}`}>
            <div className="td c-mname">
              <span className={`status-dot ${m.status === "connected" ? "dot-green" : "dot-gray"}`} />
              <div className="row-stack">
                <span className="row-title">{m.name}</span>
                <span className="row-sub">{m.id}</span>
              </div>
            </div>
            <div className="td c-mtransport"><span className="tag">{m.transport}</span></div>
            <div className="td c-mcmd code">{m.command}</div>
            <div className="td c-menv">
              {m.env && Object.keys(m.env).length > 0
                ? Object.keys(m.env).map(k => <span className="tag" key={k}>{k}=***</span>)
                : <span className="muted">—</span>}
            </div>
            <div className="td c-ops">
              <button className="btn-ghost btn-danger" onClick={() => remove(m)}>删除</button>
            </div>
          </div>
        ))}
        {servers.length === 0 && <div className="table-empty">暂无 MCP 服务 — 点右上角添加</div>}
      </div>
      {err && !adding && <div className="form-err">{err}</div>}
    </div>
  );
}
