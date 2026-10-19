import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";

// 产物目录:默认 dist(开发期验证用,不碰旧 UI);接线进二进制时由
// OUT_DIR=../web 覆盖(core/web 是 //go:embed 的嵌入根,产物必须含根部 index.html)。
// 开发服务器经 proxy 把 /api 转给本地核心实例(API_TARGET 指定端口)。
export default defineConfig({
  plugins: [vue()],
  base: "./",
  build: {
    outDir: process.env.OUT_DIR || "dist",
    emptyOutDir: true,
    // public/ 下的文件原样拷贝到产物根、不受 emptyOutDir 清空影响:
    // float.html 是 Mac 悬浮窗(NSPanel+WKWebView)经嵌入 FS 加载的资产,必须常在
  },
  server: {
    port: 5173,
    proxy: {
      "/api": {
        target: process.env.API_TARGET || "http://127.0.0.1:8711",
        changeOrigin: false,
      },
    },
  },
  // 预览(preview)继承 server.proxy:vite build 产物经哈希文件名分发,
  // 不受 WebView 缓存影响,验证一律走 preview 而非 dev
  preview: {
    port: 5174,
    proxy: {
      "/api": {
        target: process.env.API_TARGET || "http://127.0.0.1:8711",
        changeOrigin: false,
      },
    },
  },
});
