// RSS 展示格式化(逐行对齐旧 app.js)

// 零值时间(0001-01-01)表示从未抓取
export const rssTimeValid = s => !!s && !String(s).startsWith("0001-");

export function fmtAgo(iso) {
  if (!rssTimeValid(iso)) return "尚未更新";
  const t = new Date(iso).getTime();
  if (isNaN(t)) return "";
  const d = Date.now() - t;
  if (d < 60e3) return "刚刚更新";
  if (d < 3600e3) return Math.floor(d / 60e3) + " 分钟前更新";
  if (d < 86400e3) return Math.floor(d / 3600e3) + " 小时前更新";
  return Math.floor(d / 86400e3) + " 天前更新";
}

export function fmtItemDate(s) {
  if (!s) return "";
  const t = new Date(s);
  if (isNaN(t.getTime())) return s; // 无法识别的时间原样显示
  const d = new Date(s);
  const p = x => String(x).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

export function rssLinkName(l) {
  if (l.title) return l.title;
  try {
    const seg = decodeURIComponent((l.url.split("?")[0].split("/").pop() || ""));
    if (seg) return seg;
  } catch (_) { /* 编码异常用原地址 */ }
  return l.url;
}

// 条目可批量勾选的资源链接:it.links 即资源附件(不含"页面"候选——页面是 it.link,
// 单独处理);磁力链接需单独建 BT 任务,批量下载不含,故一并排除。
export const rssItemRes = it => (it.links || []).filter(l => l && l.url && !/^magnet:/i.test(l.url));
