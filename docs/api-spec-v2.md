# xagent-bridge API Specification (v2)

- 版本：`v2.0.0-draft`
- 状态：Draft
- 协议基线：内部 **A2A v1.0 数据模型**（Task / 状态机语义，不上线）；聊天协议 **AG-UI**；管理协议 **普通 REST**

---

## 0. 定位与范围

### 0.1 目标

`xagent-bridge` 以**插件**形态运行（随宿主启停，仅绑定本机回环地址），向下通过适配器（RuntimeAdapter）接入任意 agent-runtime——`claude-code`、`codex`、`openclaw` 等——向上提供一套协议统一的 API：

- `/ag-ui`：**聊天面（唯一流协议）**——AG-UI 协议端点，CopilotKit 应用侧 `CopilotRuntime` 直连，直接使用 `<CopilotChat>`、`useHumanInTheLoop` 等成熟组件（§5）
- `/tasks`、`/sessions`：**管理面**——任务与会话的普通 REST CRUD（查询、取消、HITL headless、历史）
- `/skills`、`/mcp`、`/files`：各 runtime 的 Skill / MCP / 工作区文件管理
- `/runtimes`、`/system`：统一前端所需的发现与健康检查

由此，**前端只需面向本规范开发一次**，切换 runtime 不改代码。

### 0.2 协议分层（三句话心智模型）

1. **前端聊天**：CopilotKit ⇄ **AG-UI** ⇄ bridge——流式渲染、HITL、断线恢复全部走这一条线；
2. **管理**：就是普通 REST——任务/会话查询、skills/mcp/files，无流协议；
3. **A2A**：只是 bridge **内部数据模型的名字**（Task、状态机、artifact 语义对齐 A2A v1.0），不上线、不进路径。

补充声明：

- **不实现 A2A JSON-RPC binding**；标准 A2A 客户端不能直连本 bridge。原因：A2A 官方定位是 agent↔agent（机器对机器）协议，其 client 是另一个 agent 而非人类 UI；生态中不存在可嵌入的 A2A 前端组件库（官方 SDK 全为协议客户端）。该项列入演进路线（§0.4）
- Agent Card 仍按 A2A AgentCard 结构输出（能力发现用途），extension 字段声明 binding

### 0.3 范围外（v2 不做）

- Push Notification（webhook 回调）
- Agent Card 签名（AgentCardSignature）
- 多租户 / 多用户隔离
- Bridge 之间的联邦（A2A over A2A）
- A2A 标准 JSON-RPC binding（见 §0.4）

### 0.4 演进路线（非 v2 范围）

- **标准 A2A JSON-RPC binding**（`message/stream`、`tasks/get`、`tasks/cancel` + well-known Card）：当出现真正的第三方 A2A 消费者（外部 agent、A2A SDK 客户端）时补充。因核心数据模型已与 A2A 对齐，届时只是增加一层机械的 binding 翻译，不需要返工核心。

---

## 1. 架构

```
┌────────────────────────────────────────────────────┐
│  统一前端：CopilotKit 组件（CopilotChat / HITL UI） │
└───────────────┬────────────────────────────────────┘
                │ CGP（CopilotKit 内部协议）
┌───────────────▼────────────────────────────────────┐
│        应用侧 CopilotRuntime（薄配置，零 adapter）  │
│   remote endpoint → bridge 的 /ag-ui 端点           │
└───────────────┬────────────────────────────────────┘
                │  AG-UI（POST run + SSE 事件） ← 聊天面 §5
                │  REST（/api/v1/...）          ← 管理面 §3–§4, §6–§10
┌───────────────▼────────────────────────────────────┐
│           xagent-bridge 插件                        │
│  ┌──────────────────────────────────────────────┐  │
│  │ 协议层：AG-UI 面 + REST 面 / 鉴权 / 错误模型  │  │
│  │ 事件层：内部事件接收 / 翻译给 AG-UI 面        │  │
│  │ 会话层：session ↔ context ↔ thread 映射        │  │
│  └──────────────────────────────────────────────┘  │
│              RuntimeAdapter 接口                    │
├───────────┬───────────┬────────────────────────────┤
│ claude-   │  codex    │  openclaw                  │
│ code      │  adapter  │  adapter                   │
└───────────┴───────────┴────────────────────────────┘
```

### 1.1 分层职责

| 层 | 职责 | 不做什么 |
|----|------|----------|
| 协议层 | 路径规范、参数校验、统一错误、鉴权；AG-UI 面与 REST 面 | 不理解具体 runtime |
| 事件层 | 接收适配器上报的内部事件，翻译为 AG-UI 事件流；内部 seq 仅用于日志与调试 | 不再提供独立的管理面 SSE / 事件回放 |
| 会话层 | `session_id` ↔ A2A `context_id` ↔ AG-UI `thread_id` 三元映射、消息历史持久化 | 不存储 artifact 本体（见 §6.3） |
| 适配层 | 能力声明、协议翻译、生命周期探测 | 不直接暴露原生接口 |

**关键约定：适配器只上报语义事件，不关心线上格式。** 事件如何渲染成前端流（当前只有 AG-UI 面）由协议层负责。

### 1.2 能力声明（Capabilities）

不同 runtime 能力不一（例如某 runtime 可能没有 Skill 体系）。每个 runtime 通过 capabilities 显式声明支持哪些模块，前端据此隐藏入口：

```json
{
  "capabilities": {
    "agui": true,
    "streaming": true,
    "hitl": true,
    "sessions": true,
    "skills": true,
    "mcp": true,
    "files": true,
    "mcp_hot_reload": false
  }
}
```

- `agui`：该 runtime 是否可通过 §5 的 AG-UI 端点对话（`false` 时 `/ag-ui` 返回 `501`）
- 对 `capabilities.X === false` 的模块发起调用 → `501`，`code: capability_not_supported`。**禁止**静默返回空结果假装支持。

---

## 2. 全局约定

### 2.1 基路径与 runtime 定位

- 所有业务端点前缀：`/api/v1`
- 需要 runtime 的端点一律以 **URL path** 定位：`/api/v1/{runtime_id}/...`（不支持 `X-Runtime-Id` header）
- 无需 runtime 的全局端点：`GET /api/v1/runtimes`、`/api/v1/system/*`、`/.well-known/agent-card.json`
- **实时流的唯一入口**是 §5 AG-UI 端点；管理面全部为请求-响应

`runtime_id` 必须是注册表中的已知 id，否则 `404 code=runtime_not_found`。

### 2.2 认证与信任边界

| 配置 | 行为 |
|------|------|
| `security.bind = 127.0.0.1`（默认） | 信任边界 = 本机 OS 用户，**不要求**鉴权头 |
| `security.bind` 为非回环地址 | **强制** `Authorization: Bearer <token>`，token 存于配置文件（权限 0600）；缺失或错误 → `401 code=unauthorized` |

无论何种绑定，下述操作在 token 已配置时必须校验：`/system/config`、`DELETE /files`（递归）、`/mcp` 写、`DELETE /sessions`。AG-UI 端点同样在非回环时要求鉴权。

### 2.3 统一错误模型

所有非 2xx 响应体：

```json
{
  "error": {
    "code": "runtime_offline",
    "message": "runtime 'codex' is offline (last_seen: 12s ago)",
    "request_id": "req_01HX...",
    "details": { "runtime_id": "codex" }
  }
}
```

**错误码表（v2 全量）：**

| HTTP | code | 场景 |
|------|------|------|
| 400 | `invalid_request` | 参数缺失 / 类型错误 / path 非法 / 同时提供 source 与 content |
| 401 | `unauthorized` | 非回环绑定下 token 缺失或错误 |
| 403 | `workspace_escape` | 文件 path 规范化后逃逸出 workspace root |
| 404 | `runtime_not_found` / `task_not_found` / `session_not_found` / `skill_not_found` / `mcp_server_not_found` / `file_not_found` | 资源不存在 |
| 409 | `conflict`（含子码 `skill_exists`、`mcp_server_exists`、`task_already_terminal`、`session_busy`、`input_not_pending`、`input_expired`、`directory_not_empty`、`builtin_skill`） | 状态冲突 |
| 413 | `payload_too_large` | 请求体 / 单文件超过上限 |
| 501 | `capability_not_supported` | runtime 能力未声明该模块 |
| 502 | `runtime_error` | 适配器或底层 agent 执行失败 |
| 503 | `runtime_offline` / `overloaded` | runtime 离线 / 并发任务数达 `max_concurrent_tasks` |
| 500 | `internal_error` | bridge 自身缺陷 |
| 429 | `rate_limited` | 单 runtime 写操作限流（默认 20 req/s） |

### 2.4 时间戳

所有时间字段为 **RFC3339 UTC 字符串**（与 A2A JSON 表示一致），如 `"2026-05-14T08:06:40Z"`。时长字段以 `_sec` 后缀标注秒。

### 2.5 分页

列表端点统一 query：`?limit=<1..200, 默认50>&cursor=<opaque>`；响应：

```json
{ "items": [], "next_cursor": null }
```

`next_cursor = null` 表示到底。

### 2.6 幂等

`POST /tasks`、`POST /files`、`POST /skills` 支持可选 `Idempotency-Key` 头：

- 同 key + 同 body（24h 内）→ 返回首次响应，不重复执行
- 同 key + 不同 body → `409 code=conflict`

### 2.7 通用规范

- JSON 字段一律 `snake_case`
- 2xx 写操作：创建类返回 `201` + 资源对象；删除类返回 `204` 无 body（例外见各端点）
- `null` 与字段缺失等价；本规范示例中省略的可选字段即 `null`

---

## 3. 运行时发现与管理 (`/runtimes`)

### 3.1 获取运行时列表

- **URL**: `GET /api/v1/runtimes`

- **Response** `200`:

```json
{
  "runtimes": [
    {
      "id": "claude-code",
      "name": "Claude Code CLI",
      "status": "online",
      "version": "1.0.2",
      "workspace": "/app/project",
      "last_seen": "2026-05-14T08:06:40Z",
      "capabilities": {
        "agui": true,
        "streaming": true,
        "hitl": true,
        "sessions": true,
        "skills": true,
        "mcp": true,
        "files": true,
        "mcp_hot_reload": false
      }
    },
    {
      "id": "codex",
      "name": "OpenAI Codex CLI",
      "status": "offline",
      "version": "0.9.1",
      "workspace": null,
      "last_seen": "2026-05-14T07:58:12Z",
      "capabilities": { "agui": true, "streaming": true, "hitl": true, "sessions": true, "skills": false, "mcp": true, "files": true, "mcp_hot_reload": true }
    }
  ]
}
```

- `status` 枚举：`online`（探测成功）/ `degraded`（探测成功但自检异常）/ `offline`（探测失败）/ `unknown`（尚未探测）
- 探测由 bridge 周期执行（默认 15s），`last_seen` 为最近一次成功探测时间

### 3.2 获取 Agent Card（能力名片）

两个入口，内容同源：

- `GET /api/v1/runtimes/{runtime_id}/agent-card` — 按 runtime 获取
- `GET /.well-known/agent-card.json` — 网关级 Card（A2A 标准发现位置），不带参数时返回 bridge 网关 Card，带 `?runtime_id=` 时返回对应 runtime 的 Card

- **Response** `200`（A2A AgentCard 结构）:

```json
{
  "protocol_version": "1.0.0",
  "name": "xagent-bridge / claude-code",
  "description": "Unified bridge over Claude Code runtime; chat via AG-UI",
  "url": "http://127.0.0.1:7860/api/v1/claude-code",
  "version": "1.0.2",
  "capabilities": {
    "streaming": true,
    "push_notifications": false,
    "state_transition_history": true
  },
  "default_input_modes": ["text/plain"],
  "default_output_modes": ["text/plain", "text/x-diff"],
  "skills": [],
  "extensions": [
    {
      "uri": "https://xagent-bridge.dev/spec/agui-binding/v2",
      "description": "Chat plane is AG-UI at POST {url}/ag-ui; management plane is REST",
      "required": true
    }
  ]
}
```

- `skills` 数组由 §7 的 skill 列表映射生成（仅 `enabled=true`，最多 20 条）
- Card 仅用于**能力发现**；真实对话走 §5 AG-UI，管理走 REST

---

## 4. 任务管理模块 (`/tasks`)

> 本模块的 Task / 状态机 / 对象语义**对齐 A2A v1.0**（bridge 内部通用语），但 wire 上就是普通 REST，无流协议。**实时渲染与 HITL 卡片走 §5 AG-UI 面**；本模块供查询、取消、headless HITL 与调试。

### 4.1 任务状态机（TaskState，与 A2A v1.0 一致）

```
                    ┌──────────── input 提交 ────────────┐
                    ▼                                    │
 submitted ──▶ working ──▶ input-required ──▶ working ───┤
     │            │              │                      │
     │            │              └──▶ rejected(终态)     │
     │            ├──▶ completed (终态)                  │
     │            ├──▶ failed    (终态)                  │
     │            └──▶ canceled  (终态) ◀────────────────┘
     └──────────────▶ canceled (终态)
```

- 非终态：`submitted`、`working`、`input-required`、`auth-required`（`auth-required` 在鉴权完成后回到 `working`）
- **终态**：`completed`、`failed`、`canceled`、`rejected`
- 终态不可逆；对终态任务再提交 HITL → `409 input_not_pending`；对终态任务 cancel → `409 task_already_terminal`
- 每次状态迁移都会更新 Task 对象；通过 AG-UI 面实时渲染，通过本模块 GET 查询

### 4.2 Task 对象

```json
{
  "task_id": "task_a9f8b7e6",
  "context_id": "ctx_9c21",
  "runtime_id": "claude-code",
  "status": "working",
  "status_message": "正在分析代码结构...",
  "input": {
    "message": "重构 src/utils.ts 中的认证逻辑",
    "attachments": []
  },
  "required_input": null,
  "artifacts": [
    {
      "artifact_id": "art_0001",
      "name": "code_diff",
      "type": "text/x-diff",
      "index": 0,
      "content": "..."
    }
  ],
  "error": null,
  "created_at": "2026-05-14T08:06:40Z",
  "updated_at": "2026-05-14T08:07:02Z",
  "ended_at": null
}
```

- `status` ∈ §4.1 状态机
- 终态为 `failed` 时 `error` 非空，结构同 §2.3 的 `error` 对象
- `artifacts` 为已收齐的 artifact 汇总；流式分片只出现在 AG-UI 面（§5.2）
- `required_input` 仅在 `status == "input-required"` 时非空，结构见 §4.4

### 4.3 端点总览

| 方法 | URL | 说明 |
|------|-----|------|
| POST | `/api/v1/{runtime_id}/tasks` | 提交任务 |
| GET | `/api/v1/{runtime_id}/tasks/{task_id}` | 查询任务状态 |
| GET | `/api/v1/{runtime_id}/tasks` | 任务列表 |
| POST | `/api/v1/{runtime_id}/tasks/{task_id}/inputs/{input_id}` | 提交 HITL 反馈（headless） |
| DELETE | `/api/v1/{runtime_id}/tasks/{task_id}` | 取消任务 |

> 聊天、流式渲染、HITL 卡片见 §5 AG-UI 面。

#### 4.3.1 提交任务

- **URL**: `POST /api/v1/{runtime_id}/tasks`
- **Headers**: `Idempotency-Key`（可选，§2.6）

**Request Body**:

```json
{
  "message": "重构 src/utils.ts 中的认证逻辑",
  "context_id": "ctx_9c21",
  "attachments": [
    { "name": "screenshot.png", "encoding": "base64", "content": "iVBOR..." }
  ],
  "metadata": { "source": "web-ui" }
}
```

- `message`（必填）：本轮用户输入
- `context_id`（可选）：挂到既有会话；**缺省时 bridge 自动创建新会话并在响应中返回**
- `attachments`（可选）：单文件 ≤ 5MB，总数 ≤ 10 个，超限 `413`

**Response** `201`:

```json
{
  "task_id": "task_a9f8b7e6",
  "context_id": "ctx_9c21",
  "status": "submitted",
  "created_at": "2026-05-14T08:06:40Z"
}
```

**错误**：`404 runtime_not_found` / `503 runtime_offline` / `503 overloaded`（并发达 `max_concurrent_tasks`）/ `404 session_not_found`（context_id 不存在）/ `413 payload_too_large`

> 日常聊天**不要**调此端点——走 §5 AG-UI 面，由 `CopilotRuntime` 代为提交。本端点面向 headless / 脚本 / 调试。

#### 4.3.2 查询任务状态

- **URL**: `GET /api/v1/{runtime_id}/tasks/{task_id}`
- **Response** `200`: 完整 Task 对象（§4.2）
- **错误**：`404 task_not_found` / `404 runtime_not_found`

#### 4.3.3 任务列表

- **URL**: `GET /api/v1/{runtime_id}/tasks?status=working&context_id=ctx_9c21&limit=50&cursor=...`
- `status` 可多选（重复 query key）
- **Response** `200`: `{ "items": [Task...], "next_cursor": null }`

#### 4.3.4 取消任务

- **URL**: `DELETE /api/v1/{runtime_id}/tasks/{task_id}`
- **Response** `200` + 终态 Task 对象（`status` 为 `canceled` 或原终态）
- 非终态 → adapter 发 cancel，bridge 等待状态落地（默认超时 5s，超时后 bridge 强制置 `canceled`）
- **错误**：`404 task_not_found` / `409 task_already_terminal`

### 4.4 HITL 人机介入（REST / headless）

#### 4.4.1 required_input 结构

任务进入 `input-required` 时，Task 对象的 `required_input` 字段（AG-UI 面同时映射为 `bridge_hitl` tool-call，§5.3）：

```json
{
  "input_id": "inp_7f3a",
  "type": "approval",
  "prompt": "是否允许执行 Shell 命令: npm test?",
  "options": null,
  "fields": null,
  "expires_at": "2026-05-14T08:16:40Z"
}
```

- `type` 枚举：

| type | 语义 | 响应 data 要求 |
|------|------|----------------|
| `approval` | 批准 / 拒绝 | `action` 必须是 `approve` 或 `reject` |
| `choice` | 单选 | `action=respond`，`data.choice` ∈ `options` |
| `text` | 自由文本输入 | `action=respond`，`data.text` 字符串 |
| `form` | 多字段表单 | `action=respond`，`data.values` 对象，键 ∈ `fields[].name` |

- `expires_at`：可空；过期后提交 → `409 input_expired`
- 同一任务可**串联**多次 input（每次有独立 `input_id`），由 `input_id` 精确对应

#### 4.4.2 提交反馈

- **URL**: `POST /api/v1/{runtime_id}/tasks/{task_id}/inputs/{input_id}`

**Request Body**:

```json
{
  "action": "approve",
  "data": { "feedback": "同意执行测试" }
}
```

- `action` 枚举：`approve` / `reject` / `respond` —— 是唯一真源，不存在与 `data.approved` 冗余的字段
- `approval` 类型下 `data` 可省略；`reject` 时 `data.feedback` 可选
- `respond` 时 `data` 按 4.4.1 类型表校验，缺失 → `400 invalid_request`

**Response** `200` + 最新 Task 对象（`status` 通常回到 `working`）。

**错误**：

| 场景 | 响应 |
|------|------|
| 任务不处于 `input-required` | `409 input_not_pending` |
| `input_id` 不匹配当前挂起的请求 | `409 input_not_pending` |
| 已过 `expires_at` | `409 input_expired` |
| 任务 / input 不存在 | `404 task_not_found` |

- 挂起超时策略：`input-required` 持续超过 `hitl.timeout_sec`（默认 600s）且无任何一路响应 → adapter 取消底层执行，任务置 `failed`，`error.code=hitl_timeout`
- **两路提交幂等**：AG-UI 面的 `bridge_hitl` 续传（§5.3）与本 REST 端点以 `input_id` 去重，第二次提交 → `409 input_not_pending`
- HITL 决策**持久化进会话历史**（§6.3，`role=tool` 消息，含 `action` 与 `feedback`），回看历史可见"为什么放行了 npm test"

---

## 5. AG-UI 聊天面 (`/ag-ui`)

> **bridge 唯一的流协议**，内建。CopilotKit 前端的原生对接面：应用侧只需在 `CopilotRuntime` 配置一个 remote endpoint 指向本模块，即可直接使用 `<CopilotChat>`、`useHumanInTheLoop`、Generative UI 等全部成熟组件，**无需为每个应用写自定义 adapter**。

### 5.1 协议与端点

- **URL**: `POST /api/v1/{runtime_id}/ag-ui`
- **请求体**: AG-UI Run 请求（`thread_id` / `run_id` / `messages` / `tools` / `state`，字段以 ag-ui 协议当前版本为准）
- **响应**: `Content-Type: text/event-stream`，事件流为 AG-UI 事件；`Cache-Control: no-store`；空闲期注释心跳（默认 15s）防代理掐连接
- `capabilities.agui=false` → `501 capability_not_supported`

**ID 映射（全局唯一真源）：**

| AG-UI | bridge |
|-------|--------|
| `thread_id` | `session_id` ≡ A2A `context_id` |
| `run_id` | 由 bridge 生成，回填至 `task_id` |

### 5.2 事件映射（内部事件 → AG-UI 事件）

bridge 事件层接收适配器上报（§12），按到齐顺序翻译：

| 内部事件 | AG-UI 事件 | 说明 |
|----------------|-----------|------|
| run / 任务开始 | `RUN_STARTED` | 响应首个 run 请求时发出 |
| assistant 文本产出 | `TEXT_MESSAGE_START` / `TEXT_MESSAGE_CONTENT` / `TEXT_MESSAGE_END` | 最终结果与流式中间文本 |
| 状态迁移 | `STATE_SNAPSHOT` / `STATE_DELTA` | `task.status`、`status_message` 映射进线程 state，前端可直接读 task 状态渲染进度 |
| 任务阶段（可选） | `STEP_STARTED` / `STEP_FINISHED` | 按 adapter 阶段上报 |
| artifact 分片 | `TOOL_CALL_START` / `TOOL_CALL_ARGS` / `TOOL_CALL_END` | artifact 以 tool-call 形态呈现，前端可按 `name` 分发 Generative UI |
| HITL `input-required` | `TOOL_CALL_START`（`name=bridge_hitl`） | 见 5.3 |
| 历史快照 | `MESSAGES_SNAPSHOT` | 新 run 流首重发，收敛前端状态 |
| 终态 `completed` | `RUN_FINISHED` | |
| 终态 `failed` / 连接期错误 | `RUN_ERROR` | |

- AG-UI 事件集以 ag-ui 协议最新版本为准，上表为 v2 锁定的**最小必要集**
- **断线恢复 = 快照收敛**：重连 = 以同 `thread_id` 发起新 run，bridge 在流首重发 `MESSAGES_SNAPSHOT` / `STATE_SNAPSHOT`。**不做逐事件补发**（管理面无流协议，补查询用 `GET /tasks`）

### 5.3 HITL 映射（对齐 CopilotKit `useHumanInTheLoop`）

采用 CopilotKit 对"后端无原生 interrupt"runtime 的标准模式：**前端注册 tool + 后端工具通道注入**（即 CopilotKit 文档中的 Strategy B）。完整链路：

```
agent-runtime 触发权限确认 / 人机决策
        ↓ RuntimeAdapter 上报
bridge: status=input-required, required_input={input_id, type, prompt, options, fields}
        ↓ AG-UI 面映射为一次「前端 tool 调用」事件（name=bridge_hitl）
        ↓ 前端 useHumanInTheLoop({ name: "bridge_hitl", render: ... })
用户点击 approve / 填表单 → respond(result)
        ↓ CopilotRuntime → bridge
bridge: 折叠为 §4.4.2 的 input 提交（与 REST 端点同一语义）
bridge → adapter → runtime 恢复执行，状态回 working
```

- **`bridge_hitl` tool-call 参数** = §4.4.1 的 `required_input` 结构（`input_id` / `type` / `prompt` / `options` / `fields` / `expires_at`）
- `approval` / `choice` / `text` / `form` 四种类型对应前端不同的 `render` 组件（如 `choice` 型即 CopilotKit 文档中的 TimePickerCard 用法）

**前端注册示意**（API 名以当前 CopilotKit 文档为准）：

```tsx
useHumanInTheLoop({
  agentId: "<runtime_id>",
  name: "bridge_hitl",
  description: "Bridge runtime 请求人类介入",
  parameters: z.object({
    input_id: z.string(),
    type: z.enum(["approval", "choice", "text", "form"]),
    prompt: z.string(),
    options: z.array(z.string()).nullable(),
    fields: z.array(z.object({ name: z.string(), label: z.string() })).nullable(),
  }),
  render: ({ args, respond }) => <HitlCard required={args} onSubmit={respond} />,
});
```

**回传路由规则（bridge 侧）：**

- 收到 run 请求时，若其消息序列中包含对 `bridge_hitl` 的 tool result，且对应 task 处于 `input-required` → **不新建 task**，折叠为 §4.4.2 的 input 提交，随后在同一 AG-UI 连接上继续该 task 的事件流
- 其余 run 请求 → 走新任务提交（`thread_id` 首次出现时自动创建会话）
- REST `POST .../inputs/{input_id}` 始终可用（headless / 调试 / 非 CopilotKit UI）；两路以 `input_id` 幂等去重，第二次提交 → `409 input_not_pending`

### 5.4 两面分工

| 场景 | 用哪面 |
|------|--------|
| 日常对话、流式渲染、HITL 卡片、Generative UI | AG-UI 面 |
| 任务查询 / 列表 / 取消、headless HITL | 管理面 REST（§4） |
| skills / mcp / files / sessions 管理 | 管理面 REST |
| 应用侧接入成本 | `CopilotRuntime` 一个 remote endpoint 指向 `/api/v1/{runtime_id}/ag-ui`，零自定义 adapter 代码 |

---

## 6. 会话与历史模块 (`/sessions`)

**模型映射：`session_id` ≡ A2A `context_id` ≡ AG-UI `thread_id`。** 三者是同一个值：前端在 CopilotKit 传 `thread_id`，在 REST 用 `session_id` / `context_id`，无需转换。

### 6.1 会话列表

- **URL**: `GET /api/v1/{runtime_id}/sessions?limit=50&cursor=...`
- **Response** `200`:

```json
{
  "items": [
    {
      "session_id": "ctx_9c21",
      "title": "重构认证逻辑",
      "task_count": 3,
      "last_status": "completed",
      "created_at": "2026-05-14T08:00:00Z",
      "updated_at": "2026-05-14T08:07:02Z"
    }
  ],
  "next_cursor": null
}
```

### 6.2 创建 / 重命名会话

- **创建**: `POST /api/v1/{runtime_id}/sessions`，body `{ "title": "新会话" }` → `201 {session_id, title, created_at}`（前端"新对话"按钮用，先建会话后提交任务，避免两步竞态；CopilotKit 侧对应新 `thread_id`）
- **重命名**: `PATCH /api/v1/{runtime_id}/sessions/{session_id}`，body `{ "title": "..." }` → `200 Session`
- **删除**: `DELETE /api/v1/{runtime_id}/sessions/{session_id}` → `204`
  - 该会话存在非终态任务 → `409 session_busy`（前端先 cancel）
  - 删除的是 bridge 侧记录；runtime 底层上下文是否清理由 adapter 决定（能力差异，不在协议承诺内）

### 6.3 会话消息历史

- **URL**: `GET /api/v1/{runtime_id}/sessions/{session_id}/messages?limit=100&cursor=...`
- **Response** `200`:

```json
{
  "items": [
    { "role": "user", "content": "重构 src/utils.ts", "content_type": "text/plain", "task_id": null, "artifact_ids": [], "created_at": "2026-05-14T08:06:40Z" },
    { "role": "assistant", "content": "好的，正在为您处理...", "content_type": "text/plain", "task_id": "task_a9f8b7e6", "artifact_ids": [], "created_at": "2026-05-14T08:06:41Z" },
    { "role": "tool", "content": "{\"action\":\"approve\",\"feedback\":\"同意执行测试\"}", "content_type": "application/json", "task_id": "task_a9f8b7e6", "artifact_ids": [], "created_at": "2026-05-14T08:07:02Z" }
  ],
  "next_cursor": null
}
```

- `role`：`user` / `assistant` / `tool`（HITL 决策、工具事件）
- `task_id`：该消息属于哪个任务；`artifact_ids` 关联产物（artifact 本体以任务 `artifacts` 字段获取，历史中只存引用，避免历史表膨胀）
- **持久化责任人 = bridge**：状态中间文本、终态结果、HITL 决策均由 bridge 写入历史；适配器不直接写
- 持久化时机：任务终态时一次性归档（进行中任务的中间流不逐条落库，刷新页面靠 AG-UI 面快照恢复，见 §5.2）

---

## 7. 技能管理模块 (`/skills`)

### 7.1 统一 Skill 模型

各 runtime 的 Skill 落地形式不同（目录型 SKILL.md、提示词片段等），bridge 对外只暴露归一化模型，**由 adapter 负责双向翻译**：

```json
{
  "id": "git-master",
  "name": "Git Master",
  "description": "处理复杂的 Git 操作",
  "enabled": true,
  "version": "1.0.0",
  "source": { "type": "builtin", "uri": null },
  "installed_at": "2026-05-14T08:00:00Z",
  "updated_at": "2026-05-14T08:00:00Z"
}
```

- `source.type`：`builtin`（runtime 自带）/ `file`（工作区文件）/ `url` / `embedded`（请求体 content）
- `id` 由 bridge 生成（slug 化 name），同一 runtime 内唯一

### 7.2 端点

| 方法 | URL | 说明 |
|------|-----|------|
| GET | `/api/v1/{runtime_id}/skills` | 已加载 skill 列表 |
| POST | `/api/v1/{runtime_id}/skills` | 安装 / 注册 |
| PATCH | `/api/v1/{runtime_id}/skills/{skill_id}` | 启用 / 禁用 |
| DELETE | `/api/v1/{runtime_id}/skills/{skill_id}` | 卸载 |

- `capabilities.skills=false` 的 runtime → 全部返回 `501 capability_not_supported`

#### 7.2.1 列表

- **Response** `200`: `{ "items": [Skill...] }`（数量通常小，不分页；超 500 条时启用 §2.5 分页）

#### 7.2.2 安装

- **URL**: `POST /api/v1/{runtime_id}/skills`
- **Headers**: `Idempotency-Key`（可选）

**Request Body**:

```json
{
  "name": "docker-helper",
  "description": "辅助 Docker 操作",
  "source": { "type": "url", "uri": "https://github.com/.../SKILL.md" },
  "content": null
}
```

- **`source` 与 `content` 必须恰好提供其一**，否则 `400 invalid_request`
- `source.type=url` 时由 **bridge 侧**拉取：
  - 仅允许 `http/https`
  - **禁止**解析到私网 / 回环 / link-local 地址（SSRF 防护，含 DNS rebinding：连接时按解析结果 IP 校验）
  - 响应 ≤ 512KB，超时 10s，否则 `400 invalid_request`（`details.reason=fetch_failed|forbidden_target|too_large`）
- `source.type=file` 时 uri 必须在 workspace root 内（同 §9 路径规则）

**Response** `201` + 完整 Skill 对象。
**错误**：`409 skill_exists`（同 id 已存在且非 url 重装）/ `400` 各校验失败。

#### 7.2.3 启用 / 禁用

- **URL**: `PATCH /api/v1/{runtime_id}/skills/{skill_id}`
- **Body**: `{ "enabled": false }`
- **Response** `200` + Skill 对象
- **生效语义（明确声明）**：变更对**新提交的任务**生效；已在 `working` 的任务不中断、不回溯
- **错误**：`404 skill_not_found`

#### 7.2.4 卸载

- **URL**: `DELETE /api/v1/{runtime_id}/skills/{skill_id}`
- **Response** `204`；`builtin` 来源的 skill 只允许禁用、不允许卸载 → `409 conflict`（`details.reason=builtin_skill`）
- **错误**：`404 skill_not_found`

---

## 8. MCP 配置模块 (`/mcp`)

### 8.1 归一化模型

bridge 定义**自有 schema**，adapter 翻译到各 runtime 的实际配置载体（不透传 Claude 的 `mcp.json` 原始格式）：

```json
{
  "id": "github-mcp",
  "name": "github-mcp",
  "transport": "stdio",
  "command": "npx",
  "args": ["-y", "@modelcontextprotocol/server-github"],
  "env": { "GITHUB_PERSONAL_ACCESS_TOKEN": "***" },
  "enabled": true,
  "status": "connected",
  "last_error": null,
  "updated_at": "2026-05-14T08:00:00Z"
}
```

- `transport`：`stdio` | `http` | `sse`（`http`/`sse` 用 `url` 字段替代 `command/args`）
- `status`：`connected` | `error` | `disabled` | `unknown`（runtime 未回报时）
- `capabilities.mcp_hot_reload`：配置变更能否不重启 runtime 生效

### 8.2 Secret 处理（强制）

- `env` 的**值在 GET 时一律返回 `"***"`**（脱敏），bridge 不提供任何读回明文的接口
- **写规则**：
  - `env` 字段**缺失** → 保留该 server 既有全部 secret
  - `env` 中出现新键 → 写入
  - `env` 中某键值为 `null` → **删除**该 secret
  - `env` 中某键值为 `"***"` → 视为"保持不变"

### 8.3 端点

| 方法 | URL | 说明 |
|------|-----|------|
| GET | `/api/v1/{runtime_id}/mcp` | 全部 MCP 服务 |
| POST | `/api/v1/{runtime_id}/mcp` | 新增（同 id → `409 mcp_server_exists`） |
| PUT | `/api/v1/{runtime_id}/mcp/{server_id}` | 整体替换（不存在 → `404`） |
| PATCH | `/api/v1/{runtime_id}/mcp/{server_id}` | 局部更新（`enabled` / `env` / `args` 等任一） |
| DELETE | `/api/v1/{runtime_id}/mcp/{server_id}` | 移除 |

- GET **Response** `200`: `{ "servers": [McpServer...] }`
- **POST / PUT Body**（`id` 在 POST 中可省，缺省取 `name` 的 slug；PUT 忽略 body 中的 `id`、以路径为准）：

```json
{
  "name": "github-mcp",
  "transport": "stdio",
  "command": "npx",
  "args": ["-y", "@modelcontextprotocol/server-github"],
  "env": { "GITHUB_PERSONAL_ACCESS_TOKEN": "ghp_xxx" },
  "enabled": true
}
```

- **Response** `200/201` + 完整 McpServer（`env` 已脱敏）+ 生效信息：

```json
{ "server": { "...": "..." }, "reload": { "applied": false, "reason": "restart_required" } }
```

- `capabilities.mcp_hot_reload=true` → `reload.applied=true` 立即生效；否则 `applied=false` + `reason=restart_required`，前端提示用户重启 runtime
- **错误**：`409 mcp_server_exists` / `404 mcp_server_not_found` / `501 capability_not_supported` / `502 runtime_error`（adapter 写入失败）

---

## 9. 工作区文件模块 (`/files`)

### 9.1 路径规则（强制）

- 所有 `path` 为**相对 workspace root** 的 POSIX 风格路径（`src/utils.ts`）
- 规范化（解析 `.` / `..` / 重复分隔符）后必须仍位于 workspace root 内，否则 `403 workspace_escape`
- 禁止绝对路径（以 `/` 开头）→ `400 invalid_request`
- 符号链接：**解析后二次校验**目标是否仍在 root 内，否则拒绝
- 单文件读写上限 `files.max_size_bytes`（默认 10MB），超限 `413`
- `path` 长度 ≤ 1024 字符；不接受 NUL 字节

### 9.2 端点

| 方法 | URL | 说明 |
|------|-----|------|
| GET | `/api/v1/{runtime_id}/files?path=src` | 列目录（path 缺省 = workspace root） |
| GET | `/api/v1/{runtime_id}/files/content?path=src/utils.ts` | 读文件内容 |
| POST | `/api/v1/{runtime_id}/files` | 写文件（upsert） |
| DELETE | `/api/v1/{runtime_id}/files?path=src/test.ts` | 删除文件 / 目录 |

#### 9.2.1 列目录

- **Response** `200`:

```json
{
  "path": "src",
  "entries": [
    { "name": "utils.ts", "path": "src/utils.ts", "type": "file", "size": 1024, "modified_at": "2026-05-14T08:00:00Z" },
    { "name": "api", "path": "src/api", "type": "dir", "size": null, "modified_at": "2026-05-14T07:00:00Z" }
  ],
  "next_cursor": null
}
```

- 目录项 `size=null`；默认按 `name` 升序；条目 > 500 时分页（§2.5）
- 目录不存在 → `404 file_not_found`

#### 9.2.2 读文件

- **URL**: `GET /api/v1/{runtime_id}/files/content?path=src/utils.ts&encoding=utf8`
- `encoding`：`utf8`（默认）| `base64`（二进制文件）
- **Response** `200`:

```json
{
  "path": "src/utils.ts",
  "encoding": "utf8",
  "content": "export function formatDate() { ... }",
  "size": 1024,
  "modified_at": "2026-05-14T08:00:00Z"
}
```

- 非 UTF-8 文件用 `utf8` 读取 → `400 invalid_request`（`details.reason=not_utf8`），提示改用 `base64`
- 路径是目录 → `400 invalid_request`；文件不存在 → `404 file_not_found`

#### 9.2.3 写文件（创建或修改）

- **URL**: `POST /api/v1/{runtime_id}/files`
- **Headers**: `Idempotency-Key`（可选）

**Request Body**:

```json
{
  "path": "src/utils.ts",
  "content": "export function formatDate() { /* updated */ }",
  "encoding": "utf8",
  "create_parents": true
}
```

- 同 path 已存在 → **覆盖**（upsert 语义），不存在 → 创建
- `create_parents` 默认 `true`；`false` 且父目录不存在 → `404 file_not_found`
- `encoding=base64` 时 `content` 为 base64 文本

**Response** `200`（修改）/ `201`（新建）:

```json
{ "path": "src/utils.ts", "size": 57, "created": false, "modified_at": "2026-05-14T08:10:00Z" }
```

**错误**：`403 workspace_escape` / `413 payload_too_large` / `400` 编码或路径非法

#### 9.2.4 删除

- **URL**: `DELETE /api/v1/{runtime_id}/files?path=src/test.ts`
- 删除文件 → `204`
- 删除目录：必须带 `?recursive=true`；缺省时非空目录 → `409 directory_not_empty`（空目录允许直接删），`recursive=true` 递归删除
- 目标不存在 → `404 file_not_found`（删除不幂等，保持显式反馈）

---

## 10. 系统模块 (`/system`)

### 10.1 健康检查

- **URL**: `GET /api/v1/system/health`
- **Response** `200`:

```json
{
  "status": "ok",
  "uptime_sec": 3600,
  "version": "2.0.0",
  "active_tasks": 3,
  "max_concurrent_tasks": 10,
  "runtimes": [
    { "id": "claude-code", "status": "online", "last_seen": "2026-05-14T08:06:40Z" },
    { "id": "codex", "status": "offline", "last_seen": "2026-05-14T07:58:12Z" }
  ]
}
```

- `status`：`ok`（正常）/ `degraded`（有 runtime offline 但 bridge 存活）/ `error`
- 前端启动一次调用即可拿到全局 + 各 runtime 状态

### 10.2 全局配置

- **GET** `/api/v1/system/config` → `200` 完整生效配置（`token` 字段脱敏为 `"***"` 或 `null`）
- **PUT** `/api/v1/system/config` → **整体替换**：未出现的字段**重置为默认值**，响应返回替换后的完整配置

**配置 Schema**:

```json
{
  "default_workspace": "/app",
  "max_concurrent_tasks": 10,
  "log_level": "info",
  "agui": {
    "heartbeat_interval_sec": 15
  },
  "hitl": {
    "timeout_sec": 600
  },
  "files": {
    "max_size_bytes": 10485760
  },
  "security": {
    "bind": "127.0.0.1",
    "token": null
  }
}
```

- 校验：`max_concurrent_tasks ∈ [1, 100]`，`log_level ∈ [debug, info, warn, error]`，`bind` 必须是合法 IP，`heartbeat_interval_sec ∈ [5, 60]`；非法 → `400 invalid_request`（`details.fields` 列出）
- `bind` 变更需重启 bridge 生效，响应中附 `"rebind_required": true` 标记
- **该端点必须在 token 已配置时校验 token**（§2.2）

---

## 11. 安全约定汇总

| # | 规则 | 落点 |
|---|------|------|
| 1 | 默认仅绑定 `127.0.0.1`；非回环必须开 Bearer token | §2.2 |
| 2 | secret（MCP `env`、config `token`）**只写不读**，GET 一律脱敏 | §8.2 / §10.2 |
| 3 | 文件 path 规范化 + 符号链接二次校验，防路径遍历 | §9.1 |
| 4 | Skill `source.url` 服务端拉取须过 SSRF 白名单（禁私网/回环/DNS rebinding） | §7.2.2 |
| 5 | AG-UI 流响应 `Cache-Control: no-store`，不得落共享缓存 | §5.1 |
| 6 | 所有文件 / skill / mcp 操作**限定在该 runtime 的 workspace 内**，adapter 不得越界 | §12 |
| 7 | 错误响应不回显堆栈 / 绝对宿主路径，`request_id` 供日志关联 | §2.3 |
| 8 | 并发上限防资源耗尽（`503 overloaded`） | §2.3 |
| 9 | 管理面**不暴露任何事件直通端点**（唯一实时出口是 AG-UI 面，鉴权与快照语义可控） | §1 / §5 |

---

## 12. RuntimeAdapter 开发指南

### 12.1 接口契约

```typescript
interface RuntimeAdapter {
  readonly id: string;
  capabilities(): Capabilities;

  // 生命周期
  health(): Promise<{ status: "online" | "degraded" | "offline"; version?: string }>;

  // 任务
  submit(req: { contextId: string; message: string; attachments: Attachment[] }): Promise<Task>;
  get(taskId: string): Promise<Task | null>;
  list(filter: TaskFilter): Promise<Task[]>;
  cancel(taskId: string): Promise<void>;
  respondInput(taskId: string, inputId: string, resp: HitlResponse): Promise<void>;

  // 事件上报（push 模型，协议层负责翻译成 AG-UI）
  onEvent(handler: (taskId: string, evt: AdapterEvent) => void): void;

  // 能力模块（capabilities 为 false 时可直接抛 CapabilityNotSupported）
  skills: SkillOps;
  mcp: McpOps;
  files: FileOps;
}
```

### 12.2 适配器必须遵守的不变量

1. **状态机合法**：提交的状态迁移必须符合 §4.1；非法迁移由 bridge 拒收并记 `warn`
2. **事件齐全**：每次状态迁移、每个 artifact 分片都必须上报 `AdapterEvent`；终态事件**不可缺失**（bridge 会为超时未终态的任务兜底置 `canceled` 并记 `error`）
3. **不关心线上格式**：适配器只上报语义事件；是否/如何渲染成 AG-UI 事件由协议层负责
4. **阻塞预算**：`submit/cancel/respondInput` 必须在 5s 内返回或抛错；长任务靠事件异步回报
5. **workspace 边界**：`files.*` 的一切路径解析以该 runtime 的 workspace 为根，适配器二次校验
6. **健康探测幂等**：`health()` 不得有副作用，bridge 每 15s 调用
7. **能力如实声明**：不支持的模块 `capabilities=false` 且方法抛 `CapabilityNotSupported`；禁止空实现假装成功

### 12.3 各 runtime 落地差异（参考，非规范承诺）

| 能力 | claude-code | codex | openclaw |
|------|-------------|-------|----------|
| streaming | CLI 会话流式输出 | exec 流式 | 自身事件流 |
| hitl | 权限确认弹窗 → 映射 `approval` | sandbox 审批 | 按实现 |
| skills | SKILL.md 目录体系 | 原生无 → `false` | 按实现 |
| mcp | mcp server 配置 | 支持 | 按实现 |
| mcp_hot_reload | false（需重启） | 按实现 | 按实现 |

> 上表为 adapter 作者参考；对外**一律以运行时注册后的 capabilities 实际值为准**（§3.1）。

---

## 13. 典型时序

### 13.1 前端启动（CopilotKit 应用）

```
应用启动 → CopilotRuntime({ remote endpoint → /api/v1/{rt}/ag-ui })
前端 → GET /api/v1/system/health        （一次拿全局 + runtime 状态）
前端 → GET /api/v1/runtimes              （拿 capabilities，决定 UI 入口）
前端 → GET /api/v1/{rt}/sessions         （恢复会话列表）
```

### 13.2 对话 + HITL 全链路

```
前端 CopilotChat ── runtimeUrl=/api/copilotkit ── CopilotRuntime
        │                                          │ remote endpoint
        ▼                                          ▼
            POST /api/v1/claude-code/ag-ui { thread_id, run_id, messages }
                    │
            bridge: thread→session, 提交 task, AG-UI SSE 回流
                    │
前端 ← RUN_STARTED / TEXT_MESSAGE_* / STATE_DELTA
前端 ← TOOL_CALL_START(bridge_hitl {input_id, type, prompt})
前端 → respond(approve) → run 续传（tool result）
bridge → 折叠为 §4.4.2 input 提交
前端 ← STATE_DELTA(working) → TEXT_MESSAGE_* → RUN_FINISHED(completed)
bridge → 归档会话历史（§6.3）
```

### 13.3 断线重连（AG-UI 面）

```
（连接断开，thread 上有进行中 task）
前端 → 以同 thread_id 发起新 run
bridge → 流首重发 MESSAGES_SNAPSHOT / STATE_SNAPSHOT 收敛状态
        → 继续后续事件（若 task 已终态，快照即含终态）
```

### 13.4 管理与调试（headless）

```
curl GET  /api/v1/claude-code/tasks?status=working     # 查进行中任务
curl GET  /api/v1/claude-code/tasks/task_a9f8b7e6      # 查单任务（含 required_input）
curl POST /api/v1/claude-code/tasks/task_a9f8b7e6/inputs/inp_7f3a -d '{"action":"approve"}'
curl DELETE /api/v1/claude-code/tasks/task_a9f8b7e6    # 取消
```

---

## 14. v1 → v2 变更记录

| # | 变更 | 原因 |
|---|------|------|
| 1 | 协议定位：内部 A2A 数据模型 + AG-UI 聊天面 + REST 管理面；Card 挂 `/.well-known/` | v1 声称"符合 A2A"但无消费场景；CopilotKit 原生协议是 AG-UI |
| 2 | 移除 `X-Runtime-Id` header，runtime 只走 URL path | 双通道导致缓存/CORS/日志不一致 |
| 3 | 新增任务查询 / 列表 REST（`GET /tasks`、`GET /tasks/{id}`） | v1 任务状态不可查询 |
| 4 | **管理面 SSE 整体删除**；实时流唯一入口收敛为 AG-UI 面（断线恢复 = 快照收敛） | A2A 不面向人类 UI；生态无 A2A 前端组件；双流协议是复杂度大头 |
| 5 | **路径 `/a2a/*` 改为 `/tasks/*`**；A2A 仅作为内部数据模型语义出现 | wire 上不再自称 A2A，消除"协议在哪一层"的困惑 |
| 6 | 新增 `session_id ≡ context_id ≡ thread_id` 三元映射与 sessions 模块 | v1 task 与 session 脱节，多轮对话无法工作 |
| 7 | 统一错误模型 + 全量错误码表 | v1 无任何错误响应定义 |
| 8 | HITL 改为 `{input_id, action}` 精确对应；AG-UI 面 ↔ `useHumanInTheLoop` 映射（§5.3）；REST headless 端点保留 | v1 冗余矛盾、多次挂起无法对应；对接 CopilotKit 需原生 HITL 契约 |
| 9 | 鉴权模型（loopback 信任 / 非回环强制 token）写入规范 | v1 高危接口裸奔 |
| 10 | MCP `env` secret 只写不读；schema 归一化，不再透传 `mcp.json` | v1 回显明文 token，且绑定 Claude 实现细节 |
| 11 | 文件模块：`GET /files` = 列目录、`GET /files/content` = 读；路径遍历防护；二进制 base64 | v1 读/列混用且无安全约束 |
| 12 | Skill `source/content` 二选一 + SSRF 防护 + 201 返回完整对象 | v1 优先级未定义、存在 SSRF |
| 13 | 时间戳统一 RFC3339、分页统一 cursor、幂等键、限流 | 全局一致性 |
| 14 | 新增 RuntimeAdapter 契约与不变量（§12） | "适配任意 runtime"需要可验证的接口纪律 |
| 15 | 模块路径单复数统一（`/skills`、`/files`、`/sessions`）；`/system/health` 携带 runtime 摘要 | 命名一致性、前端启动少一次调用 |
| 16 | capabilities 增加 `agui` | 聊天面可用性需可声明 |

---

*文档结束 — xagent-bridge api-spec-v2.md*
