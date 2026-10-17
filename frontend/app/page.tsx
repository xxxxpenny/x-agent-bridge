"use client";

import { useEffect, useState } from "react";
import { Chat } from "../components/Chat";
import { SkillsPage } from "../components/SkillsPage";
import { McpPage } from "../components/McpPage";
import { LlmPage } from "../components/LlmPage";
import { IconSkills, IconMcp, IconLlm } from "../components/Icons";
import {
  BRIDGE, RUNTIMES, api, authHeaders, getToken, setToken, newThreadId,
  type Session, type Skill, type Mcp, type View,
} from "../lib/types";

export default function Page() {
  const [agent, setAgent] = useState("claude-code");
  const [threadId, setThreadId] = useState(() => newThreadId());
  // 鉴权探测：null=探测中, false=bridge 开着无鉴权, true=需要 token（gate）
  const [needToken, setNeedToken] = useState<boolean | null>(null);

  // 默认 claude-code；?runtime= 可覆盖；bridge 上不存在该 runtime 时回落
  useEffect(() => {
    (async () => {
      let sel = "claude-code";
      const q = new URLSearchParams(window.location.search).get("runtime");
      if (q && RUNTIMES.some((r) => r.id === q)) sel = q;
      try {
        const res = await fetch(`${BRIDGE}/api/v1/runtimes`, { headers: authHeaders() });
        if (res.status === 401) { setNeedToken(true); return; }
        const ids: string[] = (await res.json()).runtimes?.map((r: any) => r.id) ?? [];
        if (ids.length > 0 && !ids.includes(sel)) sel = ids[0];
        setNeedToken(false);
      } catch { setNeedToken(false); /* bridge 未起，不挡页面 */ }
      setAgent(sel);
    })();
  }, []);

  if (needToken) return <TokenGate onOk={() => setNeedToken(false)} />;

  return (
    <App agent={agent} onSwitchAgent={setAgent} threadId={threadId} onSwitchThread={setThreadId} />
  );
}

/** token 门禁：bridge --token 启用后，首次打开需填入 token 才能继续 */
function TokenGate({ onOk }: { onOk: () => void }) {
  const [val, setVal] = useState(getToken());
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!val.trim()) return;
    setBusy(true); setErr("");
    setToken(val.trim());
    try {
      const res = await fetch(`${BRIDGE}/api/v1/runtimes`, { headers: authHeaders() });
      if (res.status === 401) { setErr("token 无效，请重试"); setToken(""); return; }
      onOk();
    } catch { setErr("bridge 连不上"); setToken(""); }
    finally { setBusy(false); }
  };

  return (
    <div style={{
      minHeight: "100vh", display: "flex", alignItems: "center", justifyContent: "center",
      background: "#0b0e14",
    }}>
      <form onSubmit={submit} style={{
        display: "flex", flexDirection: "column", gap: 14, width: 320,
        background: "#11141b", border: "1px solid #232735", borderRadius: 10, padding: 28,
      }}>
        <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
          <span style={{ color: "#35c7e8", fontSize: 20 }}>✳</span>
          <span style={{ color: "#e6e8ee", fontWeight: 600 }}>xagent-bridge</span>
        </div>
        <div style={{ color: "#7c8394", fontSize: 12.5, lineHeight: 1.6 }}>
          bridge 已启用 token 鉴权（--token）。输入 token 后继续使用。
        </div>
        <input
          type="password" autoFocus value={val} onChange={(e) => setVal(e.target.value)}
          placeholder="access token" data-testid="token-input"
          style={{
            padding: "9px 12px", borderRadius: 6, border: "1px solid #232735",
            background: "#0b0e14", color: "#e6e8ee", outline: "none", fontSize: 13,
          }}
        />
        {err && <div style={{ color: "#f4776a", fontSize: 12 }}>{err}</div>}
        <button type="submit" data-testid="token-submit" disabled={busy || !val.trim()} style={{
          padding: "9px 14px", borderRadius: 6, border: "none", cursor: "pointer",
          background: "#35c7e8", color: "#041015", fontWeight: 600, fontSize: 13, opacity: busy ? 0.6 : 1,
        }}>
          {busy ? "验证中…" : "进入"}
        </button>
      </form>
    </div>
  );
}

function App({
  agent, onSwitchAgent, threadId, onSwitchThread,
}: {
  agent: string; onSwitchAgent: (id: string) => void;
  threadId: string; onSwitchThread: (id: string) => void;
}) {
  const [view, setView] = useState<View>("chat");
  const [sessions, setSessions] = useState<Session[]>([]);
  const [skills, setSkills] = useState<Skill[] | null>(null);
  const [mcp, setMcp] = useState<Mcp[] | null>(null);
  const [health, setHealth] = useState<{ status: string; version: string } | null>(null);
  const [sessQuery, setSessQuery] = useState("");
  const [editing, setEditing] = useState<{ id: string; title: string } | null>(null);
  const [navOpen, setNavOpen] = useState(false);

  const refresh = async () => {
    try {
      const sess = await api(`/api/v1/${agent}/sessions`);
      setSessions((sess.items ?? []).sort((a: Session, b: Session) =>
        (b.updated_at ?? "").localeCompare(a.updated_at ?? "")));
    } catch { /* keep */ }
    try {
      const s = await api(`/api/v1/${agent}/skills`);
      setSkills(s.unsupported ? null : s.items ?? []);
    } catch { setSkills(null); }
    try {
      const m = await api(`/api/v1/${agent}/mcp`);
      setMcp(m.unsupported ? null : m.items ?? []);
    } catch { setMcp(null); }
  };

  useEffect(() => { refresh(); const t = setInterval(refresh, 5000); return () => clearInterval(t); }, [agent]);
  useEffect(() => {
    fetch(`${BRIDGE}/api/v1/system/health`).then(r => r.json())
      .then(h => setHealth({ status: h.status, version: h.version })).catch(() => {});
  }, []);

  const activeSession = sessions.find(s => s.session_id === threadId);
  const shownSessions = sessQuery.trim()
    ? sessions.filter(s =>
        s.title.toLowerCase().includes(sessQuery.trim().toLowerCase()) ||
        s.session_id.toLowerCase().includes(sessQuery.trim().toLowerCase()))
    : sessions;
  const titles: Record<View, string> = {
    chat: activeSession?.title ?? "新会话",
    skills: "Skills",
    mcp: "MCP Servers",
    llm: "LLM Gateway",
  };

  const saveTitle = async (id: string, title: string) => {
    const t = title.trim();
    setEditing(null);
    if (!t) return;
    try {
      await api(`/api/v1/${agent}/sessions/${id}`, {
        method: "PATCH", body: JSON.stringify({ title: t }),
      });
      await refresh();
    } catch { /* keep */ }
  };

  const closeNav = () => setNavOpen(false);

  return (
    <div className="shell">
      {navOpen && <div className="sidebar-backdrop" data-testid="sidebar-backdrop" onClick={closeNav} />}
      <aside className={"sidebar" + (navOpen ? " open" : "")}>
        <div className="logo">
          <span className="logo-mark">✳</span>
          <span className="logo-text">xagent-bridge</span>
          <button className="sidebar-close" data-testid="sidebar-close"
            onClick={closeNav} title="关闭菜单">×</button>
        </div>

        <div className="runtime-chip">
          <span className={`runtime-dot ${agent === "memory" ? "dot-blue" : "dot-violet"}`} />
          <select value={agent} onChange={e => onSwitchAgent(e.target.value)} data-testid="runtime-select">
            {RUNTIMES.map(r => (
              <option key={r.id} value={r.id}>{r.label} · {r.hint}</option>
            ))}
          </select>
        </div>

        <div className="section">
          <div className="section-head">
            <span>Sessions</span>
            <button className="mini-btn" data-testid="new-session"
              onClick={() => { onSwitchThread(newThreadId()); setView("chat"); closeNav(); }}>+ 新建</button>
          </div>
          <input
            className="sess-search"
            placeholder="搜索会话…"
            value={sessQuery}
            data-testid="session-search"
            onChange={e => setSessQuery(e.target.value)}
          />
          <div className="list">
            {view === "chat" && !sessQuery && !sessions.some(s => s.session_id === threadId) && (
              <div className="row active" onClick={() => setView("chat")}>
                <span className="row-title">新会话</span>
                <span className="row-badge">当前</span>
              </div>
            )}
            {shownSessions.map(s =>
              editing?.id === s.session_id ? (
                <div key={s.session_id} className="row active">
                  <input
                    className="sess-edit"
                    autoFocus
                    defaultValue={s.title === "New session" ? "" : s.title}
                    placeholder="会话标题"
                    onKeyDown={e => {
                      if (e.key === "Enter") saveTitle(s.session_id, (e.target as HTMLInputElement).value);
                      if (e.key === "Escape") setEditing(null);
                    }}
                    onBlur={e => saveTitle(s.session_id, e.target.value)}
                  />
                </div>
              ) : (
                <div key={s.session_id}
                  className={`row ${view === "chat" && s.session_id === threadId ? "active" : ""}`}
                  onClick={() => { onSwitchThread(s.session_id); setView("chat"); closeNav(); }}
                  title={s.title}>
                  <span className="row-title">{s.title || s.session_id}</span>
                  <button
                    className="row-editbtn"
                    title="重命名"
                    data-testid={`rename-${s.session_id}`}
                    onClick={e => {
                      e.stopPropagation();
                      setEditing({ id: s.session_id, title: s.title });
                    }}>✎</button>
                  {s.task_count > 0 && <span className="row-badge">{s.task_count}</span>}
                </div>
              )
            )}
            {shownSessions.length === 0 && (
              <div className="empty">{sessQuery ? "无匹配会话" : "暂无历史会话"}</div>
            )}
          </div>
        </div>

        <div className="section">
          <div className="section-head"><span>Resources</span></div>
          <div className="list">
            <div className={`row nav ${view === "skills" ? "active" : ""}`}
              data-testid="nav-skills" onClick={() => { setView("skills"); closeNav(); }}>
              <IconSkills />
              <span className="row-title">Skills</span>
              <span className="row-badge">{skills === null ? "—" : skills.length}</span>
            </div>
            <div className={`row nav ${view === "mcp" ? "active" : ""}`}
              data-testid="nav-mcp" onClick={() => { setView("mcp"); closeNav(); }}>
              <IconMcp />
              <span className="row-title">MCP</span>
              <span className="row-badge">{mcp === null ? "—" : mcp.length}</span>
            </div>
            <div className={`row nav ${view === "llm" ? "active" : ""}`}
              data-testid="nav-llm" onClick={() => { setView("llm"); closeNav(); }}>
              <IconLlm />
              <span className="row-title">LLM Gateway</span>
            </div>
          </div>
        </div>

        <div className="sidebar-footer">
          <span className={`status-dot ${health?.status === "ok" ? "dot-green" : "dot-gray"}`} />
          <span>bridge {health ? `v${health.version}` : "…"}</span>
          <span className="muted">{(BRIDGE || (typeof window !== "undefined" ? window.location.host : "same-origin")).replace(/^https?:\/\//, "")}</span>
        </div>
      </aside>

      <main className="main">
        <div className="topbar">
          <div className="topbar-left">
            <button className="nav-toggle" data-testid="nav-toggle"
              onClick={() => setNavOpen(true)} title="菜单" aria-label="打开菜单">☰</button>
            <div className="topbar-title">{titles[view]}</div>
          </div>
          <div className="topbar-badges">
            <span className="badge badge-ghost">{agent}</span>
            {view === "chat" && <span className="badge">AG-UI</span>}
          </div>
        </div>

        {view === "chat" && (
          <Chat agent={agent} threadId={threadId} onRunDone={refresh} />
        )}
        {view === "skills" && (
          <SkillsPage agent={agent} skills={skills} onChanged={refresh} />
        )}
        {view === "mcp" && (
          <McpPage agent={agent} servers={mcp} onChanged={refresh} />
        )}
        {view === "llm" && (
          <LlmPage />
        )}
      </main>
    </div>
  );
}
