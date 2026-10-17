import path from "path";
import type { NextConfig } from "next";

// 同源代理：浏览器只访问前端（:3000），/api/* 由 next 转发到本机 bridge。
// 这样手机用 http://<lan-ip>:3000 打开时，不会去连 127.0.0.1。
// BRIDGE_ORIGIN 可覆盖（start.sh 透传 BRIDGE_PORT / HOST 场景）。
const bridgeOrigin = process.env.BRIDGE_ORIGIN || "http://127.0.0.1:7860";

// 服务器/容器上 next build 偶发 Bus error（SWC 原生模块 + 低内存 / 小 /dev/shm）。
// NEXT_LOW_MEM=1 时关并行 worker，构建更稳（稍慢）。Makefile 默认开启。
const lowMem = process.env.NEXT_LOW_MEM === "1";

const nextConfig: NextConfig = {
  // 防止服务器上 /root 等多余 package-lock.json 被当成 workspace root
  outputFileTracingRoot: path.join(__dirname),
  ...(lowMem
    ? {
        experimental: {
          workerThreads: false,
          cpus: 1,
        },
      }
    : {}),
  async rewrites() {
    return [
      {
        source: "/api/:path*",
        destination: `${bridgeOrigin}/api/:path*`,
      },
    ];
  },
};

export default nextConfig;
