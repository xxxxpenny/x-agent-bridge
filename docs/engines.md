# 引擎初始化与配置指南

xagent-bridge 通过 **RuntimeAdapter** 接入引擎。每个引擎可以走两种接入方案：

| 接入方案 | 说明 | 默认 |
|----|----|----|
| **legacy** | 引擎定制实现（claude=SDK 控制协议，openclaw=Gateway WS）——**快，每回合免子进程冷启动** | ✅ 默认 |
| **ACP** | 统一 ACP 适配器，spawn 引擎的 ACP launcher（每回合有进程启动开销，~15s） | 环境变量显式开启 |

```bash
# 默认（快）：claude=legacy sdk、openclaw=legacy ws
./xagent-bridge

# 显式切到 ACP
XAGENT_ENGINE_CLAUDE=acp XAGENT_ENGINE_OPENCLAW=acp ./xagent-bridge

# 混搭：claude 用 acp、openclaw 保持 legacy
XAGENT_ENGINE_CLAUDE=acp ./xagent-bridge
```

启动日志会打印每个 runtime 实际使用的引擎：`adapter registered (acp | legacy sdk | legacy ws)`。

**引擎探测**：bridge 启动时探测本机安装情况（`claude` / `npx` / `openclaw` 的 PATH 位置），
**只注册已安装的 runtime**：

```
[probe] claude: /usr/local/bin/claude
[probe] npx: /usr/local/bin/npx
[probe] openclaw: /usr/local/bin/openclaw
claude-code adapter registered (acp)
openclaw adapter registered (acp)
```

- 未安装 → `未安装，跳过注册`（API 中不出现，前端下拉也不会有）
- claude 已装但缺 npx → acp 不可用时自动**回落 legacy sdk**
- 一个都没装且无 DEV → 启动时打印警告

---

## 0. 前置依赖

| 依赖 | 版本 | 用途 |
|------|------|------|
| Go | ≥ 1.22 | bridge 编译 |
| Node.js | ≥ 20（建议 24+） | 前端 + ACP launcher（npx） |
| Python | 3.9+ | e2e 脚本（可选） |

---

## 1. memory（零配置，演示用）

脚本适配器，**仅在 `DEV=true` 时注册**（生产环境不暴露）。发消息含 `test`/`测试`/`npm`
会触发预检式 HITL 卡片，用于验证前端与 HITL 链路。

```bash
DEV=true ./xagent-bridge                 # 启用 memory
curl -s localhost:7860/api/v1/runtimes | jq   # 应包含 memory: online
```

`make dev` 已自动带上 `DEV=true`。

---

## 2. claude-code

### 2.1 安装与登录

```bash
npm install -g @anthropic-ai/claude-code
claude            # 首次运行，按引导完成登录（浏览器 OAuth）
claude --version  # 确认可执行
```

> 登录凭据存于本机 `~/.claude/`；bridge 不接触、不存储任何 Anthropic 凭据。

### 2.2 ACP 模式（默认）

无需额外配置。bridge 会通过 `npx --yes @agentclientprotocol/claude-agent-acp`
拉起官方 ACP 适配器（首次 npx 会下载包，之后走缓存）。要求 `npx` 在 PATH 上。

验证：

```bash
curl -s localhost:7860/api/v1/runtimes | jq '.runtimes[]|select(.id=="claude-code")'
# name 应为 "Claude Code (ACP)"，status=online
```

### 2.3 legacy 模式（SDK 控制协议）

```bash
XAGENT_ENGINE_CLAUDE=legacy ./xagent-bridge
```

- HITL：CLI 以 `--permission-mode manual` 运行，危险工具触发 `can_use_tool`
  控制请求 → bridge_hitl 审批卡片（allow-once / deny）
- 多轮：首次回合捕获 CLI session_id，后续 `--resume=<id>`

### 2.4 能力验证

```bash
# 提交一轮对话（真实计费，一句话即可）
curl -s -X POST localhost:7860/api/v1/claude-code/tasks \
  -H 'Content-Type: application/json' -d '{"message":"一句话确认连通"}'
```

---

## 3. openclaw

### 3.1 安装与 onboard

```bash
npm install -g openclaw@latest        # 需 Node 24.16+ 或 26.1+
openclaw onboard --install-daemon     # 引导向导：配置模型、启动 Gateway
openclaw gateway status               # LaunchAgent 常驻，ws://127.0.0.1:18789
openclaw dashboard                    # 打开 Control UI，发消息验证模型可用
```

### 3.2 ACP 模式（默认）

openclaw **内置** `openclaw acp` 子命令，无需额外安装。bridge 直接 spawn 它。

注意：openclaw 的 ACP `session/new` 要求 `mcpServers` 字段（bridge 已自动带空数组）。

### 3.3 legacy 模式（Gateway WS）

```bash
XAGENT_ENGINE_OPENCLAW=legacy ./xagent-bridge
```

- 纯 Go WS 客户端：Ed25519 设备身份（`bridge/data/openclaw-device/`）+ v3 签名握手；
  loopback 连接通常自动批准配对
- 网关地址：`OPENCLAW_GATEWAY_URL`（默认 `ws://127.0.0.1:18789/`）；
  token 自动读取 `~/.openclaw/openclaw.json` 的 `gateway.auth.token`

### 3.4 HITL（执行审批）

要让 shell/工具审批走 bridge 卡片，需开启审批（默认 off 直接放行）：

```bash
openclaw config set tools.exec.ask always    # 所有 exec 都询问
# 或 on-miss：仅未命中 allowlist 时询问
openclaw approvals get                       # 查看生效策略（Effective Policy → ask=on）
```

生效后：agent 执行命令 → `exec.approval.requested`（legacy WS）或
`session/request_permission`（ACP）→ bridge_hitl 卡片 → 批准后真实执行。

关闭：`openclaw config set tools.exec.ask off`（bridge 降级为无审批直通）。

### 3.5 接入 LLM Gateway（模型集中切换）

把 openclaw 的模型出口指到 bridge，即可在 bridge 页面切换模型（agent 侧写死本地地址）：

```bash
# 1) bridge 注册上游 provider（在「LLM Gateway」页操作，或 API）：
#    Base URL = 你的模型服务商 /v1，填 API Key，点「拉取模型」
# 2) openclaw 添加 xagent provider（模型名固定，切换在 bridge 侧）：
openclaw config set models.providers.xagent '{
  "baseUrl": "http://127.0.0.1:7860/api/v1/llm",
  "apiKey": "xagent-bridge",
  "api": "openai-completions",
  "models": [{"id": "default", "name": "via xagent-bridge",
              "contextWindow": 1048576, "maxTokens": 131072,
              "reasoning": true, "input": ["text"]}]
}'
# 3) 允许该模型（策略白名单）
openclaw config set agents.defaults.modelPolicy.allow '["mimo/*","xagent/*"]'
# 4) 使用：
openclaw agent --message "你好" --model xagent/default --json
```

之后切换模型只需在 bridge「LLM Gateway」页改「当前生效」，openclaw 零改动。

### 3.6 claude-code 接入 LLM Gateway（Anthropic 协议）

claude-code 说 Anthropic Messages API。bridge 提供双向转换：
`POST /api/v1/llm/v1/messages` → 改写 model → 上游 OpenAI `chat/completions` → 响应/SSE 转回 Anthropic 事件。

```bash
# claude-code 指向 bridge（模型名任意，网关按激活指针改写）
export ANTHROPIC_BASE_URL=http://127.0.0.1:7860/api/v1/llm
export ANTHROPIC_AUTH_TOKEN=dummy   # 未开 --token 时任意值；开了则填 bridge token
claude
```

也可写进 `~/.claude/settings.json`：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:7860/api/v1/llm",
    "ANTHROPIC_AUTH_TOKEN": "dummy"
  }
}
```

之后在 bridge「LLM Gateway」页切换 provider/model 即可，claude-code 零改动。

---

## 4. 启动参数与环境变量

### 启动参数（CLI flag）

| 参数 | 默认 | 说明 |
|------|------|------|
| `--port` | `7860` | 监听端口 |
| `--host` | `127.0.0.1` | 绑定地址；服务器部署用 `0.0.0.0` |
| `--token` | （空=不校验） | API token；**非空时**全部 `/api/v1/*` 要求 `Authorization: Bearer <token>`（或 `?access_token=`），豁免 `GET /system/health` 与 CORS 预检 OPTIONS |

```bash
# 服务器部署（示例）
HOST=0.0.0.0 TOKEN=$(openssl rand -hex 16) make start
# 或直接：
./xagent-bridge --host 0.0.0.0 --port 7860 --token "$TOKEN"

# curl 访问
curl -H "Authorization: Bearer $TOKEN" http://SERVER:7860/api/v1/runtimes
# AG-UI SSE / 媒体类 GET 也可走 query（浏览器 EventSource 无法带 header 时）
# 前端首次打开会弹 token 门禁，填入后存 localStorage，后续请求自动携带
```

### 环境变量

| 变量 | 默认 | 说明 |
|------|------|------|
| `DEV` | （空） | `true` 时注册 memory 演示引擎 |
| `TOKEN` | （空） | `make start` / `start.sh` 透传给 `--token` |
| `HOST` | （空） | `make start` / `start.sh` 透传给 `--host` |
| `XAGENT_ENGINE_CLAUDE` | `legacy` | `legacy` \| `acp` |
| `XAGENT_ENGINE_OPENCLAW` | `legacy` | `legacy` \| `acp` |
| `XAGENT_ACP_DEBUG` | （空） | 非空时打印 ACP 帧日志 |
| `XAGENT_OPENCLAW_HELPER` | （空） | （已废弃，保留兼容） |
| `OPENCLAW_GATEWAY_URL` | `ws://127.0.0.1:18789/` | legacy WS 地址 |
| `OPENCLAW_GATEWAY_PORT` | `18789` | 健康探测端口 |
| `OPENCLAW_GATEWAY_TOKEN` | 读配置文件 | legacy WS bootstrap token |

## 5. 数据目录

```
bridge/data/
  sessions.db            会话缓存（sqlite，adapter 私有实现：标题/历史/handle 映射）
  openclaw-device/       openclaw legacy WS 设备身份 + device token
  xagent.db              LLM 网关配置（provider/激活指针）
```

路径锚定**可执行文件所在目录**，从任意 cwd 启动行为一致。

## 6. 接入新引擎（ checklist ）

1. 引擎有 ACP launcher（首选）→ `main.go` 里加一行
   `acp.NewAdapter("<name>", []string{"<launcher>"}, baseDir, dataDir)`
2. 无 ACP → 实现 `core.Adapter`（可选 SessionStore/History/Binder、Skills/MCP Provider），
   参照 `internal/runtimes/claude`（legacy）结构
3. 前端 `frontend/lib/types.ts` 的 `RUNTIMES` 数组加一项
4. 验证：`GET /api/v1/runtimes` → 提交一轮 → 检查 `GET /sessions/{id}/messages`

## 7. 故障排查

| 症状 | 排查 |
|------|------|
| runtime offline | 看启动日志的 `adapter registered`；openclaw 检查 `gateway status`；claude 检查 `which claude`/`npx` |
| openclaw 首轮很慢 | ACP 模式每次回合 spawn `openclaw acp` 进程（~15s 启动），属预期 |
| npx 拉包慢 | 预热：`npx --yes @agentclientprotocol/claude-agent-acp --help`；或配镜像 |
| HITL 卡片不弹 | claude：legacy 模式才走 can_use_tool；openclaw：确认 `tools.exec.ask=always/on-miss`（见 §3.4） |
| 模型切换不生效 | openclaw 需先配 `models.providers.xagent`（见 §3.5）；bridge LLM Gateway 需有「当前生效」指针 |
| 会话历史为空 | 确认走的 runtime 正确（历史按 runtime 隔离）；旧 JSON 缓存已自动迁移到 sessions.db |
