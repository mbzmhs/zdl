// 展示格式化与任务派生字段(逐行对齐旧 app.js 工具函数)

export function fmtBytes(n) {
  if (n === null || n === undefined || n < 0) return "—";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0, v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(v >= 100 ? 0 : v >= 10 ? 1 : 2)) + " " + units[i];
}

export const fmtSpeed = n => (n > 0 ? fmtBytes(n) + "/s" : "—");

export function fmtETA(speed, remaining) {
  if (!(speed > 0) || !(remaining > 0)) return "--:--";
  let s = Math.ceil(remaining / speed);
  if (s >= 86400 * 30) return "≥1月";
  const h = Math.floor(s / 3600); s %= 3600;
  const m = Math.floor(s / 60); s %= 60;
  if (h > 0) return `${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
  return `${m}:${String(s).padStart(2, "0")}`;
}

export function fmtTime(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  const p = x => String(x).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

export function extOf(name) {
  const m = /\.([a-z0-9]{1,5})$/i.exec(name || "");
  return m ? m[1].toLowerCase() : "";
}

export function iconClass(t) {
  if (t.kind === "hls") return "hls";
  const e = extOf(t.filename);
  if (["mp4", "mkv", "webm", "avi", "mov", "flv", "ts", "m4v", "wmv"].includes(e)) return "video";
  if (["mp3", "flac", "aac", "wav", "ogg", "m4a", "opus"].includes(e)) return "audio";
  if (["jpg", "jpeg", "png", "gif", "webp", "svg", "bmp", "ico"].includes(e)) return "img";
  if (["pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "epub", "md"].includes(e)) return "doc";
  if (["zip", "rar", "7z", "tar", "gz", "bz2", "xz", "iso"].includes(e)) return "archive";
  return "other";
}

export function iconLabel(t) {
  if (t.kind === "hls") return "HLS";
  const e = extOf(t.filename);
  return (e || "FILE").slice(0, 4);
}

// 磁力任务元数据就绪前没有文件名:标题优先用磁链 dn 显示名(元数据就绪
// 后通常即此名;超长截断、去路径非法字符),没有 dn 则直接用信息哈希。
export function magnetFallbackName(t) {
  if (!t || t.kind !== "bt" || t.filename || !/^magnet:/i.test(t.url || "")) return "";
  const dn = /(?:^|[?&])dn=([^&]*)/.exec(t.url);
  if (dn && dn[1]) {
    try {
      const name = decodeURIComponent(dn[1]).replace(/[\\/:*?"<>|]/g, " ").trim();
      if (name) return name.length > 40 ? name.slice(0, 40) + "…" : name;
    } catch (_) { /* 编码异常则用哈希 */ }
  }
  const m = /urn:btih:([0-9a-zA-Z]+)/.exec(t.url);
  return m ? m[1].toLowerCase() : "";
}

// 旧 UI 的 innerHTML 模板转义;Vue 插值自带转义,仅后批移植字符串模板时用
export const esc = s => String(s ?? "").replace(/[&<>"']/g, c =>
  ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

export const STATUS_TEXT = {
  waiting: "等待中", running: "下载中", paused: "已暂停",
  completed: "已完成", error: "失败", canceled: "已取消", seeding: "做种中",
  choosing: "待创建任务",
};
