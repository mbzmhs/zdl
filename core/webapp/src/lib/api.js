// API 访问与 SSE 基址解析(与旧 app.js 的 api()/SSE_BASE 语义一致)
export async function api(method, url, body) {
  const opt = { method, headers: {} };
  if (body !== undefined) {
    opt.headers["Content-Type"] = "application/json";
    opt.body = JSON.stringify(body);
  }
  const resp = await fetch(url, opt);
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(data.error || `HTTP ${resp.status}`);
  return data;
}

// Wails 窗口内页面源是 wails.localhost,资产服务中间件不支持 SSE 流式响应,
// EventSource 须直连核心真实端口(CORS 已放行);浏览器内若本就在核心端口上
// 则保持同源。
let SSE_BASE = "";

export function initSSEBase(cfg) {
  if (cfg && cfg.port && !location.origin.endsWith(":" + cfg.port)) {
    SSE_BASE = "http://127.0.0.1:" + cfg.port;
  }
}

export const sseURL = () => SSE_BASE + "/api/events";

// 媒体流地址同样须直连核心真实端口(Wails 窗口内页面源是 wails.localhost,
// 资产中间件不代理大流量响应;CORS 已放行,浏览器内保持同源)
export const mediaURL = path => SSE_BASE + path;
