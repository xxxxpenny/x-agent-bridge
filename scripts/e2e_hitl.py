#!/usr/bin/env python3
"""E2E: AG-UI run → bridge_hitl tool call → resume with tool result → completed."""
import json
import urllib.request

BASE = "http://127.0.0.1:7860/api/v1/memory/ag-ui"


def post(payload):
    req = urllib.request.Request(
        BASE,
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json", "Accept": "text/event-stream"},
    )
    events = []
    with urllib.request.urlopen(req, timeout=60) as resp:
        for raw in resp:
            line = raw.decode("utf-8").strip()
            if line.startswith("data: "):
                events.append(json.loads(line[6:]))
    return events


def types(events):
    return [e["type"] for e in events]


# ---- run 1: prompt triggers HITL ----
thread = "ctx_e2e_hitl"
run1 = post({
    "threadId": thread,
    "runId": "run_e2e_1",
    "tools": [], "context": [], "forwardedProps": {}, "state": {},
    "messages": [{"id": "m1", "role": "user", "content": "请帮我运行测试 npm test"}],
})
t1 = types(run1)
print("run1 events:", t1)
assert "RUN_STARTED" in t1, t1
assert "TEXT_MESSAGE_START" in t1, t1
assert "TOOL_CALL_START" in t1, t1
assert t1[-1] == "RUN_FINISHED", t1

hitl = next(e for e in run1 if e["type"] == "TOOL_CALL_START")
tool_call_id = hitl["toolCallId"]
assert hitl["toolCallName"] == "bridge_hitl", hitl
print("bridge_hitl toolCallId:", tool_call_id)

args_ev = next(e for e in run1 if e["type"] == "TOOL_CALL_ARGS")
required = json.loads(args_ev["delta"])
assert required["type"] == "approval", required
print("required_input prompt:", required["prompt"])

# ---- run 2: respond approve via tool result (useHumanInTheLoop respond path) ----
run2 = post({
    "threadId": thread,
    "runId": "run_e2e_2",
    "tools": [], "context": [], "forwardedProps": {}, "state": {},
    "messages": [
        {"id": "m1", "role": "user", "content": "请帮我运行测试 npm test"},
        {"id": "m2", "role": "assistant", "content": "",
         "toolCalls": [{"id": tool_call_id, "type": "function",
                        "function": {"name": "bridge_hitl", "arguments": args_ev["delta"]}}]},
        {"id": "m3", "role": "tool", "toolCallId": tool_call_id,
         "content": json.dumps({"approved": True, "feedback": "同意执行测试"})},
    ],
})
t2 = types(run2)
print("run2 events:", t2)
assert t2[-1] == "RUN_FINISHED", t2
texts = [e.get("delta", "") for e in run2 if e["type"] == "TEXT_MESSAGE_CONTENT"]
joined = "".join(texts)
assert "passed" in joined or "npm test" in joined, joined
print("run2 text:", joined[:80])

# ---- verify task state via REST ----
snap = [e for e in run2 if e["type"] == "STATE_SNAPSHOT"][-1]["snapshot"]
print("final task status:", snap["status"])
assert snap["status"] == "completed", snap

# ---- plain chat run ----
run3 = post({
    "threadId": "ctx_e2e_plain",
    "runId": "run_e2e_3",
    "tools": [], "context": [], "forwardedProps": {}, "state": {},
    "messages": [{"id": "m1", "role": "user", "content": "你好"}],
})
t3 = types(run3)
assert t3[0] == "RUN_STARTED" and t3[-1] == "RUN_FINISHED", t3
assert "TEXT_MESSAGE_CONTENT" in t3, t3
print("plain chat OK")

print("\nALL E2E CHECKS PASSED ✔")
