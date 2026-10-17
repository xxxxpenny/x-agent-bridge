"use client";

import { useEffect, useState } from "react";
import { api } from "../lib/types";

type Model = { id: string; name?: string };
type Provider = {
  id: string; base_url: string; api_key: string; models: Model[]; enabled: boolean;
};
type Active = { provider_id: string; model_id: string };

export function LlmPage() {
  const [providers, setProviders] = useState<Provider[] | null>(null);
  const [active, setActive] = useState<Active>({ provider_id: "", model_id: "" });
  const [selP, setSelP] = useState("");
  const [selM, setSelM] = useState("");
  const [adding, setAdding] = useState(false);
  const [form, setForm] = useState({ id: "", base_url: "", api_key: "" });
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState("");

  const load = async () => {
    try {
      const [p, a] = await Promise.all([
        api("/api/v1/llm/providers"),
        api("/api/v1/llm/active"),
      ]);
      setProviders(p.items ?? []);
      setActive(a);
      const prov = (p.items ?? []).find(x => x.id === a.provider_id);
      setSelP(a.provider_id || (p.items?.[0]?.id ?? ""));
      setSelM(a.model_id || (prov?.models?.[0]?.id ?? ""));
    } catch (e: any) { setErr(e.message); }
  };
  useEffect(() => { load(); }, []);

  const curProvider = providers?.find(p => p.id === active.provider_id);
  const curModel = curProvider?.models.find(m => m.id === active.model_id);

  const addProvider = async () => {
    setErr("");
    try {
      await api("/api/v1/llm/providers", { method: "POST", body: JSON.stringify(form) });
      setForm({ id: "", base_url: "", api_key: "" });
      setAdding(false);
      await load();
    } catch (e: any) { setErr(e.message); }
  };

  const fetchModels = async (id: string) => {
    setBusy(id);
    setErr("");
    try {
      await api(`/api/v1/llm/providers/${id}/fetch-models`, { method: "POST" });
      await load();
    } catch (e: any) { setErr(e.message); }
    setBusy("");
  };

  const activate = async () => {
    if (!selP || !selM) return;
    setErr("");
    try {
      await api("/api/v1/llm/active", {
        method: "PUT", body: JSON.stringify({ provider_id: selP, model_id: selM }),
      });
      await load();
    } catch (e: any) { setErr(e.message); }
  };

  const remove = async (id: string) => {
    if (!window.confirm(`删除 provider ${id}？`)) return;
    try {
      await api(`/api/v1/llm/providers/${id}`, { method: "DELETE" });
      await load();
    } catch (e: any) { setErr(e.message); }
  };

  if (providers === null) {
    return <div className="page"><div className="page-empty">加载中…</div></div>;
  }

  const selModels = providers.find(p => p.id === selP)?.models ?? [];

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>LLM Gateway</h1>
          <p>
            Provider（1）→ Models（N）。agent 写死本地地址 + 固定模型名，
            实际用哪个模型由这里的<b>当前生效</b>指针决定，切换即时生效。
          </p>
        </div>
        <button className="btn-primary" data-testid="add-llm-btn"
          onClick={() => setAdding(v => !v)}>
          {adding ? "取消" : "＋ 添加 Provider"}
        </button>
      </div>

      {/* 当前生效 */}
      <div className="active-model-card" data-testid="active-model">
        <div className="active-label">当前生效</div>
        <div className="active-value">
          {active.provider_id
            ? `${active.provider_id} / ${curModel?.name ?? active.model_id}`
            : "未设置 — 在下方选择并激活"}
        </div>
        <div className="active-switch">
          <select value={selP} data-testid="select-provider"
            onChange={e => {
              setSelP(e.target.value);
              const p = providers.find(x => x.id === e.target.value);
              setSelM(p?.models?.[0]?.id ?? "");
            }}>
            {providers.map(p => <option key={p.id} value={p.id}>{p.id}</option>)}
          </select>
          <select value={selM} data-testid="select-model"
            onChange={e => setSelM(e.target.value)}>
            {selModels.map(m => <option key={m.id} value={m.id}>{m.name || m.id}</option>)}
            {selModels.length === 0 && <option value="">（无模型，先拉取）</option>}
          </select>
          <button className="btn-primary" data-testid="activate-model"
            onClick={activate} disabled={!selP || !selM}>切换生效</button>
        </div>
      </div>

      {adding && (
        <div className="form-card">
          <div className="form-grid">
            <label>Provider ID<input value={form.id} placeholder="如：openai / mimo"
              data-testid="llm-pid" onChange={e => setForm({ ...form, id: e.target.value })} /></label>
            <label>API Key（只写不读）<input value={form.api_key} type="password" placeholder="sk-..."
              onChange={e => setForm({ ...form, api_key: e.target.value })} /></label>
          </div>
          <div className="form-row">
            <label>Base URL<input value={form.base_url} placeholder="https://api.xxx.com/v1"
              data-testid="llm-url" onChange={e => setForm({ ...form, base_url: e.target.value })} /></label>
          </div>
          {err && <div className="form-err">{err}</div>}
          <div className="form-actions">
            <button className="btn-primary" data-testid="add-llm-submit"
              onClick={addProvider} disabled={!form.id || !form.base_url}>保存</button>
          </div>
        </div>
      )}

      <div className="table">
        <div className="thead">
          <div className="th c-name">Provider</div>
          <div className="th c-mcmd">Base URL</div>
          <div className="th c-desc">模型</div>
          <div className="th c-menv">Key</div>
          <div className="th c-ops">操作</div>
        </div>
        {providers.map(p => (
          <div className="trow" key={p.id} data-testid={`llm-${p.id}`}>
            <div className="td c-name">
              <span className={`status-dot ${p.id === active.provider_id ? "dot-green" : "dot-gray"}`} />
              <span className="row-title">{p.id}</span>
            </div>
            <div className="td c-mcmd code">{p.base_url}</div>
            <div className="td c-desc">
              {p.models.length === 0 ? "—"
                : `${p.models.length} 个模型：${p.models.slice(0, 3).map(m => m.id).join(", ")}${p.models.length > 3 ? "…" : ""}`}
            </div>
            <div className="td c-menv"><span className="tag">{p.api_key || "—"}</span></div>
            <div className="td c-ops">
              <button className="btn-ghost" disabled={busy === p.id}
                onClick={() => fetchModels(p.id)}>
                {busy === p.id ? "拉取中…" : "拉取模型"}
              </button>
              <button className="btn-ghost btn-danger" onClick={() => remove(p.id)}>删除</button>
            </div>
          </div>
        ))}
        {providers.length === 0 && <div className="table-empty">暂无 Provider — 点右上角添加</div>}
      </div>
      {err && !adding && <div className="form-err">{err}</div>}
    </div>
  );
}
