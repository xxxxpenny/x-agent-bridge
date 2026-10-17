#!/usr/bin/env bash
# 开发模式：bridge + Next.js dev 同时启动，Ctrl-C 一起退出。
# 依赖 PATH 上的 go >= 1.22（可用 GO=/path/to/go 覆盖）
set -euo pipefail
PORT="${1:-7860}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GO="${GO:-go}"

cleanup() { kill 0 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# 版本守卫（与 Makefile check-go 同逻辑）
V="$("$GO" version | awk '{print $3}')"
case "$V" in
  go1.2[2-9]*|go1.[3-9][0-9]*|go[2-9].*) ;;
  *) echo "✗ 需要 Go >= 1.22，当前: $V ($(command -v "$GO" || echo $GO))"; exit 1;;
esac

echo "==> bridge :${PORT} (DEV=true 启用 memory 演示引擎, $V)"
(cd "$ROOT/bridge" && "$GO" build -o xagent-bridge ./cmd/xagent-bridge)
(cd "$ROOT/bridge" && DEV=true ./xagent-bridge --port "$PORT") &

echo "==> frontend :3000 (BRIDGE_URL=http://127.0.0.1:${PORT})"
(cd "$ROOT/frontend" && BRIDGE_URL="http://127.0.0.1:${PORT}" npm run dev) &

wait
