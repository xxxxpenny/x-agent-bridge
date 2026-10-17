# xagent-bridge 构建/运行入口
# 依赖：Go >= 1.22、Node >= 20（前端与 npx ACP launcher）
# 可用 GO=/path/to/go make build 覆盖工具链

BRIDGE_DIR := bridge
FRONTEND_DIR := frontend
BIN := $(BRIDGE_DIR)/xagent-bridge
PORT ?= 7860
GO ?= go
# 浏览器可达的 bridge 地址（云上部署时设置，如 BRIDGE_URL=http://<server-ip>:7860）
BRIDGE_URL ?=

.PHONY: build bridge frontend frontend-build dev start stop e2e clean fmt vet check-go

## check-go: 校验 PATH 上的 go 版本（≥ 1.22）
check-go:
	@command -v $(GO) >/dev/null 2>&1 || { \
		echo "✗ 找不到 go（GO=$(GO)）。请安装 Go >= 1.22 并确保在 PATH 上"; exit 1; }
	@V=$$($(GO) version | awk '{print $$3}'); \
	case "$$V" in go1.2[2-9]*|go1.[3-9][0-9]*|go[2-9].*) ;; \
	*) echo "✗ 需要 Go >= 1.22，当前: $$V ($$(command -v $(GO)))"; \
	   echo "  升级后重试；或指定工具链：make GO=/path/to/go <target>"; exit 1;; esac
	@echo "✓ $$($(GO) version)"

## build: 编译 bridge + 安装前端依赖
build: bridge frontend

## bridge: 仅编译 Go bridge → bridge/xagent-bridge
bridge: check-go
	cd $(BRIDGE_DIR) && $(GO) build -o xagent-bridge ./cmd/xagent-bridge
	@echo "✓ bridge → $(BIN)"

## frontend: 安装前端依赖（改动 package.json 后重复执行安全）
frontend:
	cd $(FRONTEND_DIR) && npm install
	@echo "✓ frontend deps ready"

## dev: 开发模式（bridge :$(PORT) + next dev :3000，Ctrl-C 一起退出）
dev:
	bash scripts/dev.sh $(PORT)

## start: 生产模式（bridge + 前端一起启动；Ctrl-C 一起退出）
start: bridge frontend frontend-build
	bash scripts/start.sh $(PORT)

## frontend-build: 构建前端生产包（BRIDGE_URL 非空时内联为 NEXT_PUBLIC_BRIDGE_URL）
frontend-build:
	cd $(FRONTEND_DIR) && NEXT_PUBLIC_BRIDGE_URL="$(BRIDGE_URL)" npm run build
	@echo "✓ frontend build → $(FRONTEND_DIR)/.next (bridge=$(BRIDGE_URL:-default 127.0.0.1:7860))"

## e2e: 运行 wire 级回归（需 bridge 已在 :$(PORT) 运行）
e2e:
	python3 scripts/e2e_hitl.py
	python3 scripts/e2e_openclaw_ws.py
	python3 scripts/e2e_sdk_adapter.py

fmt: check-go
	cd $(BRIDGE_DIR) && $(GO) fmt ./cmd/... ./internal/...

vet: check-go
	cd $(BRIDGE_DIR) && $(GO) vet ./...

clean:
	rm -f $(BIN)
	rm -rf $(FRONTEND_DIR)/.next
