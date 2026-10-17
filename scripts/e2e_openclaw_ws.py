#!/usr/bin/env python3
"""OpenClaw WS adapter e2e: streaming deltas + real exec-approval HITL."""
import json, os, time, urllib.request

BASE = "http://127.0.0.1:7860/api/v1/openclaw"
AGUI = BASE + "/ag-ui"

def agui_run(payload, timeout=300):
    req = urllib.request.Request(AGUI, data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json", "Accept": "text/event-stream"})
    events = []
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        for raw in resp:
            line = raw.decode().strip()
            if line.startswith("data: "):
                events.append(json.loads(line[6:]))
    return events

# ---- 1) streaming: multiple text deltas over time ----
print("=== streaming ===")
t0 = time.time()
evs = agui_run({
    "threadId": "ctx_oc_stream", "runId": "r1",
    "tools": [], "context": [], "forwardedProps": {}, "state": {},
    "messages": [{"id": "m1", "role": "user", "content": "写一段80字左右的短文，主题：山间清晨。"}],
})
counts = {}
samples = []
chars = 0
for e in evs:
    counts[e["type"]] = counts.get(e["type"], 0) + 1
    if e["type"] == "TEXT_MESSAGE_CONTENT":
        chars += len(e.get("delta", ""))
        samples.append((round(time.time()-t0, 1), chars))
print("events:", counts)
n = len(samples)
print("delta arrival: first", samples[:3], "… last", samples[-2:] if n else None)
assert counts.get("TEXT_MESSAGE_CONTENT", 0) >= 3, "no token-level streaming!"
assert evs[-1]["type"] == "RUN_FINISHED"
print(f"✓ streaming: {counts['TEXT_MESSAGE_CONTENT']} deltas, {chars} chars")

# ---- 2) HITL: touch file needs exec approval ----
print()
print("=== HITL ===")
proof = "/tmp/xagent-oc-ws-hitl"
if os.path.exists(proof):
    os.remove(proof)
content = "请使用 shell 工具执行命令 touch " + proof + " 创建文件。必须真正执行，然后简短确认。"
thread = "ctx_oc_hitl"
history = [{"id": "m1", "role": "user", "content": content}]
hitl_seen = 0
for rnd in range(1, 6):
    evs = agui_run({
        "threadId": thread, "runId": f"r_{rnd}",
        "tools": [], "context": [], "forwardedProps": {}, "state": {},
        "messages": history,
    })
    types = [e["type"] for e in evs]
    hitls = [e for e in evs if e["type"] == "TOOL_CALL_START" and e.get("toolCallName") == "bridge_hitl"]
    out = "".join(e.get("delta", "") for e in evs if e["type"] == "TEXT_MESSAGE_CONTENT")
    snap = [e for e in evs if e["type"] == "STATE_SNAPSHOT"][-1]["snapshot"]
    print(f"round {rnd}: status={snap['status']} hitl={len(hitls)} text={out[:60]!r}")
    if not hitls:
        assert snap["status"] == "completed", snap
        break
    hitl_seen += len(hitls)
    for h in hitls:
        tcid = h["toolCallId"]
        args = next(e["delta"] for e in evs if e["type"] == "TOOL_CALL_ARGS" and e.get("toolCallId") == tcid)
        print("  approval:", json.loads(args).get("command", "?"))
        history.append({"id": f"a_{tcid[:8]}", "role": "assistant", "content": "",
                        "toolCalls": [{"id": tcid, "type": "function",
                                       "function": {"name": "bridge_hitl", "arguments": args}}]})
        history.append({"id": f"t_{tcid[:8]}", "role": "tool", "toolCallId": tcid,
                        "content": json.dumps({"approved": True, "feedback": "允许执行"})})
else:
    raise SystemExit("HITL did not complete in 5 rounds")

assert hitl_seen > 0, "no bridge_hitl seen!"
assert os.path.exists(proof), "file not created after approval"
print("✓ HITL: approval → exec allowed → file created")
print()
print("OPENCLAW WS E2E PASSED ✔")
