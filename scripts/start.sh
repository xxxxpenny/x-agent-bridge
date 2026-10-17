#!/usr/bin/env bash
# 生产模式：bridge + 前端（next start）同时前台运行，Ctrl-C 一起退出。
# 前置：make bridge frontend frontend-build（make start 已自动处理）
#   用法:  scripts/start.sh [port]
#   环境:  TOKEN=<token>     透传 --token（启用 API 鉴权；前端打开需填 token）
#          HOST=<addr>       透传 --host（服务器部署用 0.0.0.0；默认 127.0.0.1）
#          BRIDGE_URL=<url>  可选：跨源直连 bridge（构建期内联 NEXT_PUBLIC_BRIDGE_URL）
#          BRIDGE_ORIGIN=<url> 可选：next 代理目标，默认 http://127.0.0.1:$PORT
set -euo pipefail
PORT="${1:-7860}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GO="${GO:-go}"
FRONTEND_PORT="${FRONTEND_PORT:-3000}"

cleanup() { kill 0 2>/dev/null || true; }
trap cleanup EXIT INT TERM

V="$("$GO" version | awk '{print $3}')"
case "$V" in
  go1.2[2-9]*|go1.[3-9][0-9]*|go[2-9].*) ;;
  *) echo "✗ 需要 Go >= 1.22，当前: $V"; exit 1;;
esac

EXTRA_ARGS=()
[ -n "${TOKEN:-}" ] && EXTRA_ARGS+=(--token "$TOKEN")
[ -n "${HOST:-}" ]  && EXTRA_ARGS+=(--host "$HOST")

echo "==> bridge  ${HOST:-127.0.0.1}:${PORT}  ($V, token=${TOKEN:+enabled}${TOKEN:-off})"
(cd "$ROOT/bridge" && "$GO" build -o xagent-bridge ./cmd/xagent-bridge)
(cd "$ROOT/bridge" && ./xagent-bridge --port "$PORT" "${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}") &

echo "==> frontend http://localhost:${FRONTEND_PORT}  (proxy /api/* → ${BRIDGE_ORIGIN:-http://127.0.0.1:${PORT}})"
(cd "$ROOT/frontend" && BRIDGE_ORIGIN="${BRIDGE_ORIGIN:-http://127.0.0.1:${PORT}}" PORT="$FRONTEND_PORT" npm run start) &

echo "（Ctrl-C 停止全部）"
wait
