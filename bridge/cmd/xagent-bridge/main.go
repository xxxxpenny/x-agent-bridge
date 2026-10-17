// xagent-bridge: 统一 agent 网关（AG-UI 聊天面 + REST 管理面）。
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"xagent-bridge/internal/core"
	"xagent-bridge/internal/llmgw"
	"xagent-bridge/internal/persist"
	"xagent-bridge/internal/runtimes/acp"
	"xagent-bridge/internal/runtimes/claude"
	"xagent-bridge/internal/runtimes/memory"
	ocleg "xagent-bridge/internal/runtimes/openclaw"
	"xagent-bridge/internal/server"
)

func main() {
	port := flag.String("port", "7860", "listen port")
	host := flag.String("host", "127.0.0.1", "bind address（服务器部署用 0.0.0.0）")
	token := flag.String("token", "", "API token；非空时全接口要求 Authorization: Bearer <token>（health 除外）")
	flag.Parse()

	// 相对路径锚定可执行文件目录（bridge/ 或项目根启动都一致）
	exe, _ := os.Executable()
	baseDir := filepath.Dir(exe)
	dataDir := filepath.Join(baseDir, "data")

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	db, err := persist.Open(filepath.Join(dataDir, "xagent.db"))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	store := core.NewStore()

	// ---- 引擎探测：只注册机器上已安装的 runtime ----
	claudeBin, claudeErr := exec.LookPath("claude")
	npxBin, npxErr := exec.LookPath("npx")
	openclawBin, openclawErr := exec.LookPath("openclaw")
	probe := func(name, bin string, err error) {
		if err != nil || bin == "" {
			log.Printf("[probe] %s: 未安装", name)
		} else {
			log.Printf("[probe] %s: %s", name, bin)
		}
	}
	probe("claude", claudeBin, claudeErr)
	probe("npx", npxBin, npxErr)
	probe("openclaw", openclawBin, openclawErr)

	adapters := map[string]*core.AdapterRuntime{}
	// memory 演示引擎仅在 DEV=true 时注册（生产不暴露）
	if os.Getenv("DEV") == "true" {
		adapters["memory"] = &core.AdapterRuntime{ID: "memory", Adapter: memory.NewAdapter(), Workspace: baseDir}
		log.Printf("memory adapter registered (dev)")
	}
	// 引擎选择：默认走老链路（定制实现，快）；ACP 通过环境变量显式开启
	// （ACP 每回合 spawn 子进程，启动慢）：
	//   XAGENT_ENGINE_CLAUDE   = legacy | acp   （默认 legacy）
	//   XAGENT_ENGINE_OPENCLAW = legacy | acp   （默认 legacy）
	engineFor := func(name string) string {
		v := strings.ToUpper(os.Getenv("XAGENT_ENGINE_" + name))
		if v == "ACP" {
			return "acp"
		}
		return "legacy"
	}

	// ---- claude-code ----
	if claudeBin == "" {
		log.Printf("claude-code: 未安装，跳过注册")
	} else if engineFor("CLAUDE") == "legacy" {
		if ca := claude.NewAdapter(baseDir, dataDir); ca != nil {
			adapters["claude-code"] = &core.AdapterRuntime{ID: "claude-code", Adapter: ca, Workspace: baseDir}
			log.Printf("claude-code adapter registered (legacy sdk)")
		} else {
			log.Printf("claude-code legacy adapter unavailable")
		}
	} else if npxBin == "" {
		log.Printf("claude-code: 缺少 npx，acp 不可用 → 回落 legacy sdk")
		if ca := claude.NewAdapter(baseDir, dataDir); ca != nil {
			adapters["claude-code"] = &core.AdapterRuntime{ID: "claude-code", Adapter: ca, Workspace: baseDir}
			log.Printf("claude-code adapter registered (legacy sdk)")
		}
	} else if ca := acp.NewAdapter("claude-code",
		[]string{npxBin, "--yes", "@agentclientprotocol/claude-agent-acp"}, baseDir, dataDir); ca != nil {
		adapters["claude-code"] = &core.AdapterRuntime{ID: "claude-code", Adapter: ca, Workspace: baseDir}
		log.Printf("claude-code adapter registered (acp)")
	} else {
		log.Printf("claude-code acp adapter unavailable")
	}

	// ---- openclaw ----
	if openclawBin == "" {
		log.Printf("openclaw: 未安装，跳过注册")
	} else if engineFor("OPENCLAW") == "legacy" {
		if oa := ocleg.NewAdapter(baseDir); oa != nil {
			adapters["openclaw"] = &core.AdapterRuntime{ID: "openclaw", Adapter: oa, Workspace: baseDir}
			log.Printf("openclaw adapter registered (legacy ws)")
		}
	} else if oa := acp.NewAdapter("openclaw", []string{openclawBin, "acp"}, baseDir, dataDir); oa != nil {
		adapters["openclaw"] = &core.AdapterRuntime{ID: "openclaw", Adapter: oa, Workspace: baseDir}
		log.Printf("openclaw adapter registered (acp)")
	} else {
		log.Printf("openclaw acp adapter unavailable")
	}

	if len(adapters) == 0 {
		log.Printf("警告：未注册任何 runtime（安装 claude-code / openclaw，或 DEV=true）")
	}

	srv := server.New(store, adapters, llmgw.NewService(db), *token, dataDir)
	addr := *host + ":" + *port
	if *token != "" {
		log.Printf("token auth enabled (health/OPTIONS exempt)")
	}
	log.Printf("xagent-bridge listening on http://%s", addr)
	// model proxy 配置用：OpenAI 兼容，/{base}/chat/completions 与 /{base}/v1/chat/completions 均可
	log.Printf("llm gateway baseUrl: http://%s/api/v1/llm  (OpenAI chat/completions + Anthropic /v1/messages；host=0.0.0.0 时云上换成服务器 IP/域名)", addr)
	log.Fatal(http.ListenAndServe(addr, srv.Handler()))
}
