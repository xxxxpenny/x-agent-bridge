#!/usr/bin/env python3
"""Verify SDK-style claude adapter: multi-turn resume + real can_use_tool HITL."""
import json
import os
import time
import urllib.request

BASE = "http://127.0.0.1:7860/api/v1/claude-code"


def rest(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as r:
        if r.status == 204:
            return None
        return json.loads(r.read())


def wait_task(tid, timeout=120):
    t0 = time.time()
    while time.time() - t0 < timeout:
        t = rest("GET", f"/tasks/{tid}")
        if t["status"] in ("completed", "failed", "canceled"):
            return t
        time.sleep(1.5)
    raise TimeoutError(tid)


print("=== Test 1: multi-turn session resume ===")
ctx = "ctx_sdk_mem"
t1 = rest("POST", "/tasks", {"message": "我叫小明，记住这个名字，回复确认即可", "context_id": ctx})
r1 = wait_task(t1["task_id"])
print("turn1:", r1["status"], "|", (r1.get("status_message") or "")[:40])
assert r1["status"] == "completed", r1

t2 = rest("POST", "/tasks", {"message": "我叫什么名字？只答名字", "context_id": ctx})
r2 = wait_task(t2["task_id"])
assert r2["status"] == "completed", r2
msgs = rest("GET", f"/sessions/{ctx}/messages")["items"]
assistant = [m for m in msgs if m.get("role") == "assistant"]
last = assistant[-1]["content"] if assistant else ""
print("turn2 reply:", last[:80])
assert "小明" in last, f"resume failed, reply={last!r}"
print("✓ multi-turn resume OK (claude remembered 小明)")


print()
print("=== Test 2: real can_use_tool HITL via AG-UI (multi-round) ===")
AGUI = "http://127.0.0.1:7860/api/v1/claude-code/ag-ui"


def agui_run(payload):
    req = urllib.request.Request(
        AGUI, data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json", "Accept": "text/event-stream"})
    events = []
    with urllib.request.urlopen(req, timeout=300) as resp:
        for raw in resp:
            line = raw.decode().strip()
            if line.startswith("data: "):
                events.append(json.loads(line[6:]))
    return events


thread = "ctx_sdk_hitl"
content = "使用 Bash 工具执行 touch /tmp/xagent-sdk-hitl 创建文件，然后列出它确认。每一步必须真正执行。"
history = [{"id": "m1", "role": "user", "content": content}]
hitl_seen = 0
for rnd in range(1, 6):
    events = agui_run({
        "threadId": thread, "runId": f"run_sdk_{rnd}",
        "tools": [], "context": [], "forwardedProps": {}, "state": {},
        "messages": history,
    })
    types = [e["type"] for e in events]
    hitls = [e for e in events if e["type"] == "TOOL_CALL_START" and e.get("toolCallName") == "bridge_hitl"]
    out = "".join(e.get("delta", "") for e in events if e["type"] == "TEXT_MESSAGE_CONTENT")
    snap = [e for e in events if e["type"] == "STATE_SNAPSHOT"][-1]["snapshot"]
    print(f"round {rnd}: status={snap['status']} hitl={len(hitls)}")
    if not hitls:
        assert snap["status"] == "completed" and hitl_seen > 0, (snap, types[-5:])
        assert os.path.exists("/tmp/xagent-sdk-hitl"), "touch did not run"
        print("✓ HITL approve → touch executed → completed")
        break
    hitl_seen += len(hitls)
    for h in hitls:
        tcid = h["toolCallId"]
        args = next(e["delta"] for e in events
                    if e["type"] == "TOOL_CALL_ARGS" and e.get("toolCallId") == tcid)
        print("  approved:", json.loads(args).get("prompt", "")[:70])
        history.append({"id": f"a_{tcid[:8]}", "role": "assistant", "content": "",
                        "toolCalls": [{"id": tcid, "type": "function",
                                       "function": {"name": "bridge_hitl", "arguments": args}}]})
        history.append({"id": f"t_{tcid[:8]}", "role": "tool", "toolCallId": tcid,
                        "content": json.dumps({"approved": True, "feedback": "允许执行"})})
else:
    raise SystemExit("HITL did not complete in 5 rounds")

print()
print("ALL SDK-ADAPTER CHECKS PASSED ✔")
