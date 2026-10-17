"use client";

import { useEffect, useRef, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { BRIDGE, authHeaders, uploadFiles, type Attachment, type AttChip, type ChatMsg, type PendingHitl } from "../lib/types";
import { consumeAgUI, parseHitlInfo } from "../lib/agui";

export function Chat({ agent, threadId, onRunDone }: {
  agent: string; threadId: string; onRunDone: () => Promise<void>;
}) {
  const [msgs, setMsgs] = useState<ChatMsg[]>([]);
  const [input, setInput] = useState("");
  const [streaming, setStreaming] = useState(false);
  const [pendingHitl, setPendingHitl] = useState<PendingHitl | null>(null);
  const [chips, setChips] = useState<AttChip[]>([]);
  const [attMap, setAttMap] = useState<Record<string, Attachment>>({});
  const [dragOver, setDragOver] = useState(false);
  const abortRef = useRef<AbortController | null>(null);
  const scroller = useRef<HTMLDivElement>(null);
  const lastUserRef = useRef("");
  const fileInput = useRef<HTMLInputElement>(null);
  const chipSeq = useRef(0);

  // 切换 thread → 拉 bridge 历史
  useEffect(() => {
    let dead = false;
    setMsgs([]);
    setStreaming(false);
    setPendingHitl(null);
    setChips([]);
    setAttMap({});
    abortRef.current?.abort();
    fetch(`${BRIDGE}/api/v1/${agent}/sessions/${threadId}/messages`, { headers: authHeaders() })
      .then(r => (r.ok ? r.json() : { items: [] }))
      .then(d => {
        if (dead) return;
        const loaded: ChatMsg[] = (d.items ?? []).map((m: any, i: number) => {
          if (m.role === "tool") {
            let note = "已处理审批请求";
            try {
              const c = typeof m.content === "string" ? JSON.parse(m.content) : m.content;
              const mark = c.action === "reject" ? "✗ 已拒绝" : "✓ 已批准";
              note = c.summary ? `${mark} · ${c.summary}` : mark;
            } catch { /* legacy */ }
            return { id: `h${i}_${m.created_at ?? i}`, role: "note" as const, text: note, done: true };
          }
          return {
            id: `h${i}_${m.created_at ?? i}`,
            role: m.role === "user" ? "user" as const : "assistant" as const,
            text: typeof m.content === "string" ? m.content : JSON.stringify(m.content),
            thinking: typeof m.thinking === "string" ? m.thinking : "",
            done: true,
            attachments: Array.isArray(m.attachments) ? m.attachments : undefined,
          };
        });
        setMsgs(loaded);
      })
      .catch(() => { /* bridge down */ });
    return () => { dead = true; };
  }, [agent, threadId]);

  // ---- 附件：拖/贴/选 三入口统一走 addFiles ----
  const addFiles = async (files: File[]) => {
    if (files.length === 0 || streaming) return;
    const seq = ++chipSeq.current;
    const placeholders: AttChip[] = files.map((f, i) => ({
      id: `up_${seq}_${i}`, name: f.name, size: f.size, uploading: true,
    }));
    setChips(cs => [...cs, ...placeholders]);
    try {
      const items = await uploadFiles(agent, threadId, files);
      setAttMap(m => {
        const next = { ...m };
        for (const a of items) next[a.id] = a;
        return next;
      });
      setChips(cs => cs.map(c => {
        const hit = items.find((_, i) => c.id === `up_${seq}_${i}`);
        return hit ? { id: hit.id, name: hit.name, size: hit.size } : c;
      }));
    } catch (e: any) {
      const msg = String(e?.message ?? e);
      setChips(cs => cs.map(c =>
        placeholders.some(p => p.id === c.id) ? { ...c, uploading: false, error: msg } : c));
    }
  };

  const onDrop = (e: React.DragEvent) => {
    e.preventDefault();
    setDragOver(false);
    const files = Array.from(e.dataTransfer.files ?? []);
    if (files.length > 0) { addFiles(files); return; }
    // 拖文字（如代码片段）→ 落入输入框
    const text = e.dataTransfer.getData("text/plain");
    if (text) setInput(inp => (inp ? inp + "\n" + text : text));
  };
  const onPaste = (e: React.ClipboardEvent<HTMLTextAreaElement>) => {
    const files = Array.from(e.clipboardData?.files ?? []);
    if (files.length > 0) { e.preventDefault(); addFiles(files); }
  };
  const removeChip = (id: string) => setChips(cs => cs.filter(c => c.id !== id));
  const readyCount = chips.filter(c => !c.uploading && !c.error).length;
  const canSend = !streaming && !pendingHitl && !chips.some(c => c.uploading) && (!!input.trim() || readyCount > 0);

  useEffect(() => {
    scroller.current?.scrollTo({ top: scroller.current.scrollHeight });
  }, [msgs]);

  const patch = (id: string, fn: (m: ChatMsg) => ChatMsg) =>
    setMsgs(ms => ms.map(m => (m.id === id ? fn(m) : m)));

  const handleEvent = (id: string, ev: any) => {
    switch (ev.type) {
      case "REASONING_MESSAGE_CONTENT":
        patch(id, m => ({ ...m, thinking: (m.thinking ?? "") + (ev.delta ?? "") }));
        break;
      case "TEXT_MESSAGE_CONTENT":
        patch(id, m => ({ ...m, text: m.text + (ev.delta ?? "") }));
        break;
      case "TOOL_CALL_START":
        if (ev.toolCallName === "bridge_hitl") {
          // 审批弹在输入框上方；关闭当前 assistant，续跑写入新消息
          patch(id, m => ({ ...m, done: true }));
          setPendingHitl({
            inputId: ev.toolCallId, argsJson: "", userText: lastUserRef.current,
          });
        }
        break;
      case "TOOL_CALL_ARGS":
        setPendingHitl(p => {
          if (!p || p.inputId !== ev.toolCallId) return p;
          return { ...p, argsJson: p.argsJson + (ev.delta ?? "") };
        });
        break;
      case "RUN_ERROR":
        patch(id, m => ({ ...m, error: ev.message ?? "run error", done: true }));
        break;
    }
  };

  /** 一次 AG-UI run 的通用生命周期 */
  const doRun = async (assistantId: string, payloadMsgs: any[], atts?: Attachment[]) => {
    const ctrl = new AbortController();
    abortRef.current = ctrl;
    setStreaming(true);
    try {
      await consumeAgUI({
        agent, threadId, messages: payloadMsgs, attachments: atts,
        signal: ctrl.signal, onEvent: ev => handleEvent(assistantId, ev),
      });
    } catch (e: any) {
      if (e?.name !== "AbortError") {
        patch(assistantId, m => ({ ...m, error: String(e?.message ?? e), done: true }));
      }
    } finally {
      setStreaming(false);
      abortRef.current = null;
      patch(assistantId, m => ({ ...m, done: true }));
      onRunDone().catch(() => {});
    }
  };

  const send = async () => {
    const text = input.trim();
    const ready = chips.filter(c => !c.uploading && !c.error);
    if ((!text && ready.length === 0) || streaming || pendingHitl) return;
    if (chips.some(c => c.uploading)) return; // 上传中不发
    setInput("");
    lastUserRef.current = text;
    const atts = ready.map(c => attMap[c.id]).filter(Boolean);
    const shownAtts = ready.map(c => ({ id: c.id, name: c.name, size: c.size }));
    setChips([]);
    const aid = "a" + Date.now();
    setMsgs(ms => [
      ...ms,
      { id: "u" + Date.now(), role: "user", text: text || "（发送了文件）", done: true,
        attachments: shownAtts.length > 0 ? shownAtts : undefined },
      { id: aid, role: "assistant", text: "", thinking: "", done: false },
    ]);
    await doRun(aid, [{ id: "m" + Date.now(), role: "user", content: text || "请分析我上传的文件" }], atts);
  };

  const respondHITL = async (p: PendingHitl, approved: boolean) => {
    if (streaming) return;
    setPendingHitl(null);
    const info = parseHitlInfo(p.argsJson);
    const brief = info.command || (info.prompt ? info.prompt.split("\n")[0] : "");
    const aid2 = "a" + Date.now();
    setMsgs(ms => [...ms,
      {
        id: "n" + Date.now(), role: "note", done: true,
        text: approved
          ? `✓ 已批准执行 ${info.toolName}${brief ? "：" + brief : ""}`
          : `✗ 已拒绝执行 ${info.toolName}`,
      },
      { id: aid2, role: "assistant", text: "", thinking: "", done: false },
    ]);

    const payloadMsgs = [
      { id: "u1", role: "user", content: p.userText },
      { id: "a1", role: "assistant", content: "",
        toolCalls: [{ id: p.inputId, type: "function",
          function: { name: "bridge_hitl", arguments: p.argsJson } }] },
      { id: "t1", role: "tool", toolCallId: p.inputId,
        content: JSON.stringify(approved
          ? { approved: true, feedback: "用户已批准" }
          : { approved: false, decision: "reject", feedback: "用户拒绝了此操作" }) },
    ];
    await doRun(aid2, payloadMsgs);
  };

  return (
    <div className={"chat-wrap" + (dragOver ? " drag-over" : "")}
      onDragOver={e => { e.preventDefault(); setDragOver(true); }}
      onDragLeave={e => {
        if (e.currentTarget === e.target) setDragOver(false);
      }}
      onDrop={onDrop}
      data-testid="chat-wrap">
      <div className="chat-scroll" ref={scroller}>
        <div className="chat-inner">
          {msgs.length === 0 && (
            <div className="chat-empty">
              <div className="chat-empty-mark">✳</div>
              <div>开始新对话 — 消息会写入当前 session（SQLite 持久化）</div>
              <div className="chat-empty-hints">
                <button onClick={() => setInput("你好，介绍一下你自己")}>打个招呼</button>
                <button onClick={() => setInput("使用 Bash 工具 touch /tmp/demo-hello 创建文件")}>触发 HITL</button>
              </div>
              <div className="chat-empty-hints chat-empty-att">也可拖入 / 粘贴 / 点击 📎 上传文件</div>
            </div>
          )}
          {msgs.map(m => <Message key={m.id} m={m} />)}
          {streaming && <div className="typing"><span></span><span></span><span></span></div>}
        </div>
      </div>

      {dragOver && <div className="drag-overlay" data-testid="drag-overlay">松开以上传文件</div>}

      <div className="chat-inputbar">
        {pendingHitl && <HitlTray p={pendingHitl} disabled={streaming} onRespond={respondHITL} />}
        {chips.length > 0 && (
          <div className="att-chips" data-testid="att-chips">
            {chips.map(c => (
              <span key={c.id}
                className={"att-chip" + (c.error ? " att-chip-err" : "") + (c.uploading ? " att-chip-up" : "")}
                data-testid="att-chip">
                <span className="att-chip-icon">📎</span>
                <span className="att-chip-name">{c.name}</span>
                <span className="att-chip-size">{fmtSize(c.size)}</span>
                {c.error && <span className="att-chip-errmsg" title={c.error}>失败</span>}
                <button className="att-chip-x" data-testid="att-chip-remove"
                  onClick={() => removeChip(c.id)} title="移除">×</button>
              </span>
            ))}
          </div>
        )}
        <div className="chat-inputbox">
          <input ref={fileInput} type="file" multiple hidden data-testid="file-input"
            onChange={e => {
              const files = Array.from(e.target.files ?? []);
              e.target.value = "";
              if (files.length > 0) addFiles(files);
            }} />
          <button className="attach-btn" data-testid="attach-btn" disabled={streaming || !!pendingHitl}
            onClick={() => fileInput.current?.click()} title="添加附件">📎</button>
          <textarea
            value={input}
            rows={1}
            placeholder={pendingHitl ? "等待审批结果…" : "输入消息，Enter 发送…支持拖入/粘贴文件"}
            disabled={streaming || !!pendingHitl}
            onChange={e => setInput(e.target.value)}
            onKeyDown={e => {
              if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); }
            }}
          />
          {streaming ? (
            <button className="send-btn stop" data-testid="stop-btn"
              onClick={() => abortRef.current?.abort()} title="停止">■</button>
          ) : (
            <button className="send-btn" data-testid="send-btn"
              disabled={!canSend} onClick={send} title="发送">↑</button>
          )}
        </div>
      </div>
    </div>
  );
}

function fmtSize(n: number): string {
  if (n < 1024) return n + "B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + "KB";
  return (n / 1024 / 1024).toFixed(1) + "MB";
}

/** 审批托盘：附着在输入框上方，不在消息流里插卡片 */
function HitlTray({ p, disabled, onRespond }: {
  p: PendingHitl; disabled: boolean;
  onRespond: (p: PendingHitl, approved: boolean) => Promise<void>;
}) {
  const info = parseHitlInfo(p.argsJson);
  return (
    <div className="hitl-tray" data-testid="hitl-tray">
      <div className="hitl-tray-head">
        <span className="hitl-shield">🛡</span>
        <div>
          <div className="hitl-tool">{info.toolName} 请求执行权限</div>
          {info.prompt && <div className="hitl-desc">{info.prompt.split("\n")[0]}</div>}
        </div>
      </div>
      {info.command && <pre className="hitl-cmd" data-testid="hitl-cmd">{info.command}</pre>}
      {!info.command && info.inputPretty && (
        <pre className="hitl-cmd">{info.inputPretty}</pre>
      )}
      <div className="hitl-tray-actions">
        <button data-testid="hitl-reject" className="btn-reject"
          disabled={disabled} onClick={() => onRespond(p, false)}>拒绝</button>
        <button data-testid="hitl-approve" className="btn-approve"
          disabled={disabled} onClick={() => onRespond(p, true)}>批准执行</button>
      </div>
    </div>
  );
}

function Message({ m }: { m: ChatMsg }) {
  if (m.role === "user") {
    return (
      <div className="msg-user" data-testid="msg-user">
        <div className="msg-user-bubble">
          {m.text && <div>{m.text}</div>}
          {m.attachments && m.attachments.length > 0 && (
            <div className="msg-atts" data-testid="msg-atts">
              {m.attachments.map(a => (
                <span key={a.id} className="msg-att" title={a.name}>
                  <span>📎</span>{a.name}<em>{fmtSize(a.size)}</em>
                </span>
              ))}
            </div>
          )}
        </div>
      </div>
    );
  }
  if (m.role === "note") {
    return <div className="msg-note" data-testid="msg-note">{m.text}</div>;
  }
  const thinkingOpen = !m.done;
  return (
    <div className="msg-assistant" data-testid="msg-assistant">
      {m.thinking ? (
        <details className="msg-think" open={thinkingOpen} data-testid="msg-thinking">
          <summary>💭 思考过程</summary>
          <div className="msg-think-body">{m.thinking}</div>
        </details>
      ) : null}
      {m.text && (
        <div className="md" data-testid="msg-text">
          <ReactMarkdown remarkPlugins={[remarkGfm]}>{m.text}</ReactMarkdown>
        </div>
      )}
      {m.error && <div className="msg-error">⚠ {m.error}</div>}
    </div>
  );
}
