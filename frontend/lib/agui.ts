import { BRIDGE, authHeaders, type Attachment } from "./types";

/** 解析 bridge_hitl 的 args：提取工具名与人话入参（Bash → 命令） */
export function parseHitlInfo(argsJson: string): {
  toolName: string; prompt: string; command: string; inputPretty: string;
} {
  let toolName = "", prompt = "";
  try {
    const a = JSON.parse(argsJson || "{}");
    toolName = a.tool_name || "";
    prompt = a.prompt || "";
  } catch { /* partial args */ }
  if (!toolName && prompt.includes("工具")) {
    const m = prompt.match(/工具\s*(\S+?)\s*吗/);
    if (m) toolName = m[1];
  }
  let command = "", inputPretty = "";
  const m = prompt.match(/入参:\s*([\s\S]+)$/);
  if (m) {
    try {
      const input = JSON.parse(m[1]);
      if (typeof input.command === "string") command = input.command;
      else inputPretty = JSON.stringify(input, null, 2);
    } catch { inputPretty = m[1].trim(); }
  }
  if (!toolName) toolName = "命令";
  return { toolName, prompt, command, inputPretty };
}

/**
 * 消费 bridge 的 AG-UI SSE 流：POST run + 解析 data: 帧，逐事件回调。
 * Chat 的新消息与 HITL 续跑共用这一条实现。
 */
export async function consumeAgUI(opts: {
  agent: string;
  threadId: string;
  messages: any[];
  attachments?: Attachment[];
  signal: AbortSignal;
  onEvent: (ev: any) => void;
}) {
  const runId = "run_" + Math.random().toString(36).slice(2, 12);
  const res = await fetch(`${BRIDGE}/api/v1/${opts.agent}/ag-ui`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "text/event-stream", ...authHeaders() },
    signal: opts.signal,
    body: JSON.stringify({
      threadId: opts.threadId,
      runId,
      tools: [],
      context: [],
      forwardedProps: {},
      state: {},
      messages: opts.messages,
      attachments: opts.attachments ?? [],
    }),
  });
  if (!res.ok || !res.body) throw new Error(`ag-ui ${res.status}`);

  const reader = res.body.getReader();
  const dec = new TextDecoder();
  let buf = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += dec.decode(value, { stream: true });
    const frames = buf.split("\n\n");
    buf = frames.pop() ?? "";
    for (const frame of frames) {
      for (const line of frame.split("\n")) {
        if (!line.startsWith("data: ")) continue;
        let ev: any;
        try { ev = JSON.parse(line.slice(6)); } catch { continue; }
        opts.onEvent(ev);
      }
    }
  }
}
