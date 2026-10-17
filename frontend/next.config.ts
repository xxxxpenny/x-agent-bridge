import type { NextConfig } from "next";

// 同源代理：浏览器只访问前端（:3000），/api/* 由 next 转发到本机 bridge。
// 这样手机用 http://<lan-ip>:3000 打开时，不会去连 127.0.0.1。
// BRIDGE_ORIGIN 可覆盖（start.sh 透传 BRIDGE_PORT / HOST 场景）。
const bridgeOrigin = process.env.BRIDGE_ORIGIN || "http://127.0.0.1:7860";

const nextConfig: NextConfig = {
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
