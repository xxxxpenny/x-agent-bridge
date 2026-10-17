# xagent-bridge

统一 Agent 网关 spike：Go 实现 bridge，适配任意 agent-runtime，前端 CopilotKit 直连。

```
CopilotKit 组件 ── CGP ── CopilotRuntime ── AG-UI (POST + SSE) ── bridge (Go :7860)
                                                                      │
                                              MemoryAdapter / ClaudeCodeAdapter
```

协议与设计见 [docs/api-spec-v2.md](docs/api-spec-v2.md)。本仓库是其 spike 实现：
聊天面走 **AG-UI**（§5），管理面为 REST（§4），HITL 走 `bridge_hitl` 前端 tool（§5.3）。

## 目录

```
bridge/
  cmd/xagent-bridge/        入口（组装 core + persist + runtimes + llmgw + server）
  internal/core/            契约层：Adapter 接口（含 SessionStore/SessionHistory/
                            SessionBinder 会话能力）、Task 运行时模型、投影器
  internal/persist/         仅 LLM 网关配置（SQLite）；会话存储已下沉 adapter
  internal/llmgw/           LLM Gateway：OpenAI 兼容代理 + Provider(1:N) 模型切换
  internal/server/          HTTP 面：REST + AG-UI；会话/历史走 adapter 接口
  internal/runtimes/
    memory/                 脚本演示（会话=内存实现）
    sesscache/              共享会话缓存（thread→handle + 标题 + 消息索引）
    acp/                    ACP 通用适配器（默认；claude-code/openclaw 共用）
    claude/                 老链路：SDK 控制协议（XAGENT_ENGINE_CLAUDE=legacy）
    openclaw/               老链路：Gateway WS（XAGENT_ENGINE_OPENCLAW=legacy）
frontend/                   Next.js：app/ 外壳 + components/ + lib/
scripts/e2e_*.py            wire 级回归（memory、claude ACP、openclaw ACP）
docs/                       api-spec-v2.md + 验证截图
```

**LLM Gateway（Provider 1:N + 页面切换）**：
- Provider 注册：`/api/v1/llm/providers`（base_url + key + **拉取模型列表**）
- **当前生效指针**：`/api/v1/llm/active`（页面上选 provider/model 一键切换，即时生效）
- 代理：`POST /api/v1/llm/chat/completions`（及 openclaw 用的无 `/v1` 变体）——
  agent 侧写死本地地址 + 固定模型名（如 `xagent/mimo`），代理按激活指针改写 model 转发上游
- **Anthropic Messages**：`POST /api/v1/llm/v1/messages`（claude-code `ANTHROPIC_BASE_URL` 指这里）——
  网关把 Anthropic 协议转成上游 OpenAI 格式（含 SSE 流事件翻译），agent 零改动换模型
- 已实测：openclaw `xagent/mimo` → bridge → mimo 上游；页面/接口切换 v2.6-pro ⇄ v2.6-flash 零改动 agent

**新增引擎只需两步**：实现 `core.Adapter` 接口（可选 `SkillsProvider`/`MCPProvider`/Admin），在 `cmd/xagent-bridge/main.go` 注册一行。构建：`go build -o xagent-bridge ./cmd/xagent-bridge`

## Runtimes

| id | 协议 | HITL | 多轮 | 说明 |
|----|------|------|------|------|
| claude-code | **ACP**（`npx @agentclientprotocol/claude-agent-acp`） | ✅ request_permission | ✅ session/load | 统一 ACP 适配器 |
| openclaw | **ACP**（内置 `openclaw acp`） | ✅ request_permission | ✅ session/load | 统一 ACP 适配器 |
| memory | 脚本 | ✅ 预检式 | — | 演示用 |

**ACP 通用适配器**（`internal/runtimes/acp`）：一个 Go 实现（JSON-RPC over stdio）服务所有
ACP coding agent —— 流式（thought/message chunk）、真 HITL（`session/request_permission` ↔
`{outcome: selected}`）、多轮（session-id 映射 + `session/load`）。接入新引擎 = 在
`cmd/xagent-bridge/main.go` 注册一行 launcher 命令。调试 ACP 帧：`XAGENT_ACP_DEBUG=1 ./xagent-bridge`

**双引擎可切换**（ACP 只是接入方案之一，老链路为定制实现，一等公民）：

| runtime | 老链路（默认，快） | ACP（可选，spawn 慢） |
|----|------|------|
| claude-code | SDK 控制协议（can_use_tool 真 HITL） | `npx @agentclientprotocol/claude-agent-acp` |
| openclaw | Gateway WS（Ed25519 握手 + exec.approval） | 内置 `openclaw acp` |

```bash
# 默认走老链路；ACP 需显式开启
XAGENT_ENGINE_CLAUDE=acp XAGENT_ENGINE_OPENCLAW=acp ./xagent-bridge
```

两条链路都实现 SessionStore/SessionHistory/SessionBinder（共享 `internal/runtimes/sesscache`：
SQLite 会话缓存——thread→native handle + 标题 + 消息索引，adapter 私有实现）。

## 快速开始

```bash
# 依赖：Go 1.22+、Node 20+

make build        # 编译 bridge + 安装前端依赖
make dev          # 开发模式：bridge :7860 + frontend :3000（Ctrl-C 一起退出）
make e2e          # wire 级回归（需 bridge 已运行）

# 生产式启动
make start                  # 前台运行 bridge + 前端（Ctrl-C 一起退出）

# 服务器部署：非回环绑定 + token 鉴权（浏览器经 :3000 同源代理访问 bridge，无需 BRIDGE_URL）
HOST=0.0.0.0 TOKEN=$(openssl rand -hex 16) make start
# 或直接：./xagent-bridge --host 0.0.0.0 --token "$TOKEN"
# curl: -H "Authorization: Bearer $TOKEN" ... ；前端打开会弹 token 门禁，填入即可
```

**服务器 `next build` Bus error？** `make frontend-build` 默认已开 `NEXT_LOW_MEM=1`
（关 SWC 并行 worker）+ `NODE_OPTIONS=--max-old-space-size=4096`。
若仍崩溃：Docker 需 `--shm-size=1g`；确认 Node ≥ 20；本机可 `NEXT_LOW_MEM=0 make frontend-build` 恢复全速构建。

引擎初始化（claude-code 登录 / openclaw onboard / HITL 审批配置 / LLM Gateway 接入）见
**[docs/engines.md](docs/engines.md)**；HTTP 协议细节见 [docs/api-spec-v2.md](docs/api-spec-v2.md)。

打开 http://localhost:3000：

- **左栏**（a13n console 风格）：runtime 切换 chip、**Sessions** 内联列表（点击切 thread、+ 新建、搜索/重命名）、**Resources 导航**（Skills / MCP / LLM Gateway → 右侧管理页）、bridge 状态
- **聊天页**（自绘，AG-UI 直连 bridge）：markdown 渲染、thinking 折叠块、历史回显（含 thinking）
- **HITL**：审批请求在**输入框上方托盘**弹出（工具名 + 命令代码块 + 批准/拒绝）；结果以友好 pill 展示并持久化
- **管理页**：Skills（安装/启停/删除）· MCP（读写 .mcp.json，密钥脱敏）· LLM Gateway（Provider 1:N + 当前生效模型切换）
- 默认 runtime = claude-code（需已登录，见 engines.md）；memory 演示引擎仅 `DEV=true`（make dev 自动开启）

## 验证状态（2026-10-16）

| 项 | 状态 |
|----|------|
| AG-UI wire HITL 全环（e2e_hitl.py，memory） | ✅ 全绿 |
| 浏览器 UI HITL（CopilotKit → bridge → 批准 → 续跑） | ✅ 已验证（docs/spike-hitl-passed.png） |
| claude-code 流式（token 级 delta + thinking 透传） | ✅ 线级 25%@8.4s…100%@23.2s；UI 打字机效果 |
| **claude-code 多轮记忆**（thread → `--resume=<session-id>`） | ✅ e2e：跨轮记住"小明" |
| **claude-code 真·中途 HITL**（SDK 控制协议 can_use_tool） | ✅ e2e：多轮审批后 touch/ls 真实执行（e2e_sdk_adapter.py） |
| 双栏 UI（sessions 内联 + skills/mcp 管理页） | ✅（docs/ui-claude-code-chat.png、ui-skills-admin.png） |
| Skills/MCP 管理写接口 | ✅ |

### ClaudeCodeAdapter（对齐 claude-agent-sdk-python transport）

- 双向 stream-json：`--input-format stream-json`，prompt 走 stdin `{"type":"user",...}`
- `control_request {subtype:"initialize"}` 握手后再发 prompt
- **真·中途 HITL**：`--permission-prompt-tool stdio` + `--permission-mode manual` →
  危险工具触发 `can_use_tool` → bridge_hitl 卡片 → `control_response`（allow/deny）
  （注意：只读命令 ls/echo/cat 在白名单内不会触发询问；CLI 2.x 无 "default" 模式，取值为 `manual`）
- 多轮：首次捕获 CLI `session_id` 存入 session 映射，后续同 thread 用 `--resume=<id>`（等号形式，防 flag 注入）
- `CLAUDE_CODE_ENTRYPOINT=sdk-py`，剥离 `CLAUDECODE` 环境变量

### 已知限制（spike）

- 点击历史会话会切换 thread（后续消息写入该会话），但**聊天区不渲染旧历史**（v2 CopilotKit 无 `initialMessages`，bridge 未实现线程历史下发）；REST `GET /sessions/{id}/messages` 有完整记录
- Skills 仅 `project` 作用域可改（`~/.claude/skills` 用户级只读）；MCP 变更需 runtime 重启加载 `.mcp.json`
- `/api/copilotkit/info` 探活 404 告警（单路由模式，不影响聊天）

## 关键约定（与 spec §5.3 一致）

- `thread_id ≡ session_id ≡ context_id`
- HITL：bridge 发 `TOOL_CALL_START(bridge_hitl)`，`toolCallId == input_id`；
  前端 `useHumanInTheLoop` respond 后，CopilotRuntime 发**新 run**，消息含 `role:"tool"` 结果，
  bridge 折叠为 input 提交并续跑同 task
- 断线恢复 = 快照收敛（新 run 首发 `STATE_SNAPSHOT`），不做逐事件补发

## ClaudeCodeAdapter 备注

- 会话映射（thread → claude session_id）、消息与 **thinking（reasoning 全文）** 均持久化到 `bridge/data/xagent.db`（SQLite WAL），bridge 重启后完整恢复
- HITL 的 REST/AG-UI 折叠两路都实测可用；单任务内可多轮审批（每次独立 input_id）。
- 之前的 prompt 层预检 HITL 已被 SDK can_use_tool 方案替换（MemoryAdapter 仍保留预检式演示）。
