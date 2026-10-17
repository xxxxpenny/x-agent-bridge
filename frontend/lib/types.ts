// 默认同源（空字符串）：fetch 走相对路径 /api/*，由 next rewrites 代理到 bridge。
// 这样手机/云端用任意 IP 打开 :3000 都不需要重新构建。
// 需要跨源直连 bridge 时，构建时注入 NEXT_PUBLIC_BRIDGE_URL（浏览器可达地址）。
export const BRIDGE = process.env.NEXT_PUBLIC_BRIDGE_URL || "";

// ---- token（bridge --token 启用鉴权时，前端门禁填入并持久化）----
const TOKEN_KEY = "xagent_token";
export function getToken(): string {
  if (typeof window === "undefined") return "";
  return localStorage.getItem(TOKEN_KEY) ?? "";
}
export function setToken(t: string) {
  if (typeof window === "undefined") return;
  if (t) localStorage.setItem(TOKEN_KEY, t);
  else localStorage.removeItem(TOKEN_KEY);
}
export function authHeaders(): Record<string, string> {
  const t = getToken();
  return t ? { Authorization: `Bearer ${t}` } : {};
}

export const RUNTIMES = [
  { id: "claude-code", label: "claude-code", hint: "真实 CLI" },
  { id: "openclaw", label: "openclaw", hint: "Gateway" },
  { id: "memory", label: "memory", hint: "脚本演示" },
];

export type View = "chat" | "skills" | "mcp" | "llm";
export type Session = {
  session_id: string; runtime_id?: string; title: string; task_count: number; updated_at: string;
};
export type Skill = { id: string; name: string; description: string; enabled: boolean; source: string };
export type Mcp = { id: string; name: string; transport: string; command: string; status: string; env?: Record<string, string> };
export type LlmRoute = {
  alias: string; upstream_url: string; upstream_model: string; api_key: string; enabled: boolean;
};

export type PendingHitl = { inputId: string; argsJson: string; userText: string };
export type Attachment = { id: string; name: string; path: string; size: number };
/** 待发送附件（上传已完成）/ 上传中占位 */
export type AttChip = { id: string; name: string; size: number; uploading?: boolean; error?: string };

/** 上传附件到 bridge（multipart 字段 file，可多个）→ 元数据列表 */
export async function uploadFiles(agent: string, threadId: string, files: File[]): Promise<Attachment[]> {
  const fd = new FormData();
  for (const f of files) fd.append("file", f, f.name);
  const res = await fetch(`${BRIDGE}/api/v1/${agent}/sessions/${threadId}/attachments`, {
    method: "POST", headers: authHeaders(), body: fd,
  });
  if (!res.ok) {
    let msg = `${res.status}`;
    try { msg = (await res.json())?.error?.message ?? msg; } catch { /* */ }
    throw new Error(msg);
  }
  return (await res.json()).items ?? [];
}
export type ChatMsg = {
  id: string;
  role: "user" | "assistant" | "note";
  text: string;
  thinking?: string;
  done?: boolean;
  error?: string;
  attachments?: { id: string; name: string; size: number }[];
};

export function newThreadId() {
  return "ctx_" + Math.random().toString(36).slice(2, 10) + Date.now().toString(36).slice(-4);
}

/** bridge 公共 REST 封装：501 → unsupported；非 2xx 抛错 */
export async function api(path: string, init?: RequestInit) {
  const res = await fetch(`${BRIDGE}${path}`, {
    ...init,
    headers: { "Content-Type": "application/json", ...authHeaders(), ...(init?.headers ?? {}) },
  });
  if (res.status === 401) throw new Error("unauthorized");
  if (res.status === 501) return { unsupported: true };
  if (!res.ok) {
    let msg = `${res.status}`;
    try { msg = (await res.json())?.error?.message ?? msg; } catch { /* */ }
    throw new Error(msg);
  }
  if (res.status === 204) return null;
  return res.json();
}
