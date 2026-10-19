/* ZDownloader 助手 —— MV3 Service Worker
 * 职责:下载捕获、媒体嗅探、右键菜单、与核心通信。
 */

const DEFAULTS = {
  enabled: true,        // 总开关
  server: "http://127.0.0.1:8620",
  minSizeMB: 1,         // 直接媒体小于该值不接管(0 = 全部)
  extBlacklist: "htm,html,php,asp,aspx,jsp,shtml,css,js", // 忽略的扩展名
  domainBlacklist: "",  // 忽略的域名(逗号/换行分隔)
  captureMedia: true,   // 自动接管直接媒体(视频/音频文件)
  notify: true,         // 失败通知(接管失败时必须让用户知道)
  notifySuccess: false, // 成功通知(默认关闭,成功仅角标闪烁,防止刷屏)
};

const PLAYLIST_CT = /(mpegurl|dash\+xml)/i;
const PLAYLIST_EXT = /\.(m3u8|mpd)(\?|#|$)/i;
const MEDIA_CT = /^(video|audio)\//i;
const MEDIA_EXT = /\.(mp4|m4v|mkv|webm|flv|avi|mov|ts|m2ts|mp3|m4a|aac|flac|wav|ogg|opus|wma)(\?|#|$)/i;
const SNIFF_LIMIT = 30;

/* ---------------- 捕获去重与通知聚合 ---------------- */

// 同一 URL 5 分钟内只接管一次,防止 Chrome 重复派发同一资源导致风暴。
// 判断(recentlyCaptured)是纯函数,不写时间戳;打标(markCaptured)只在真正成功接管后调用。
// 拆开是关键:否则「另存为」延迟投递(onCreated 判一次→onChanged 再判一次,同一 URL 间隔
// 不足 5 分钟)会被自己第一次的写入误判为"重复"而吞掉,下载照样交给浏览器。
const recentCaptures = new Map(); // url -> ts
function recentlyCaptured(url) {
  const now = Date.now();
  const ts = recentCaptures.get(url);
  return !!(ts && now - ts < 5 * 60 * 1000);
}
function markCaptured(url) {
  const now = Date.now();
  recentCaptures.set(url, now);
  if (recentCaptures.size > 300) {
    for (const [k, v] of recentCaptures) {
      if (now - v > 5 * 60 * 1000) recentCaptures.delete(k);
    }
  }
}

// 成功通知聚合:3 秒窗口内的多次接管合并为一条,避免连环弹窗
const aggState = { count: 0, lastName: "", timer: null };
function notifyCaptureOk(name) {
  aggState.count++;
  aggState.lastName = name;
  if (aggState.timer) clearTimeout(aggState.timer);
  aggState.timer = setTimeout(async () => {
    const st = await getSettings();
    if (!st.notifySuccess) { // 默认成功不通知,仅角标
      aggState.count = 0;
      return;
    }
    const n = aggState.count;
    notify("已交给 ZDownloader",
      n > 1 ? `已接管 ${n} 个下载(最近 ${Math.round((aggState.windowMs || 3000) / 1000)} 秒)` : aggState.lastName);
    aggState.count = 0;
  }, 3000);
  // 记录窗口长度供文案使用
  aggState.windowMs = 3000;
}

/* ---------------- 设置 ---------------- */
async function getSettings() {
  const stored = await chrome.storage.local.get(DEFAULTS);
  return stored;
}
function serverBase(st) {
  return (st.server || DEFAULTS.server).replace(/\/+$/, "");
}
function isOurServer(st, url) {
  return url.startsWith(serverBase(st));
}

/* ---------------- 工具 ---------------- */
function basename(p) {
  if (!p) return "";
  const norm = p.replace(/\\/g, "/");
  const i = norm.lastIndexOf("/");
  return i >= 0 ? norm.slice(i + 1) : norm;
}
// dirname 由 Chrome 下载项的完整保存路径取所在目录(兼容 Windows 反斜杠):
// 「另存为」选定的路径形如 C:\Users\...\Downloads\a.mp4,dirname 即 C:\Users\...\Downloads。
function dirname(p) {
  if (!p) return "";
  const norm = p.replace(/\\/g, "/");
  const i = norm.lastIndexOf("/");
  return i > 0 ? norm.slice(0, i) : "";
}
function extOf(urlOrName) {
  const clean = urlOrName.split("?")[0].split("#")[0];
  const m = /\.([a-z0-9]{1,6})$/i.exec(basename(clean));
  return m ? m[1].toLowerCase() : "";
}
function inList(list, value) {
  return list.split(/[,，\n]/).map(s => s.trim().toLowerCase()).filter(Boolean)
    .some(v => value.toLowerCase().includes(v));
}

function getHeader(headers, name) {
  const h = (headers || []).find(x => x.name.toLowerCase() === name.toLowerCase());
  return h ? h.value : "";
}

async function cookiesFor(url) {
  try {
    const jars = await chrome.cookies.getAll({ url });
    return jars.map(c => `${c.name}=${c.value}`).join("; ");
  } catch (e) {
    return "";
  }
}

/* ---------------- 与核心通信 ---------------- */
async function sendToCore(st, task) {
  try {
    const resp = await fetch(serverBase(st) + "/api/tasks", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(task),
    });
    if (!resp.ok) return { ok: false, error: "核心返回 HTTP " + resp.status };
    const data = await resp.json();
    if (data.errors && data.errors.length && !(data.tasks || []).length) {
      return { ok: false, error: data.errors[0] };
    }
    return { ok: true, id: data.tasks && data.tasks[0] && data.tasks[0].id };
  } catch (e) {
    return { ok: false, error: "无法连接 ZDownloader 核心" };
  }
}

async function pingCore(st) {
  try {
    const resp = await fetch(serverBase(st) + "/api/ping");
    if (!resp.ok) return null;
    return await resp.json();
  } catch (e) {
    return null;
  }
}

// openCoreDialog 请核心弹「新建任务」窗:核心唤窗并记入持久待办,前端取走弹窗,
// 自动填 URL、预填 Referer/Cookie,其余(目录/文件名/线程)用户在窗口里自填。
// 核心未更新(旧二进制无 /api/ui/newtask → 404)或离线时,回退旧的直接建任务
// (下到核心默认目录),保证下载不被打断;核心离线则原样返回失败交由调用方提示。
async function openCoreDialog(st, payload) {
  const body = {
    urls: payload.urls,
    referer: payload.referer || undefined,
    headers: payload.headers && Object.keys(payload.headers).length ? payload.headers : undefined,
    stay: !!payload.stay,
  };
  try {
    const resp = await fetch(serverBase(st) + "/api/ui/newtask", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!resp.ok) throw new Error("核心返回 HTTP " + resp.status);
    const data = await resp.json();
    if (!data || data.ok !== true) throw new Error((data && data.error) || "核心拒绝");
    return { ok: true, dialog: true };
  } catch (e) {
    // 回退直接建任务(兼容旧核心/未更新):仍交给 ZDownloader,只是不弹窗
    const task = { urls: payload.urls, url: (payload.urls && payload.urls[0]) || undefined, referer: payload.referer || undefined };
    if (payload.headers && Object.keys(payload.headers).length) task.headers = payload.headers;
    const res = await sendToCore(st, task);
    if (res.ok) return { ok: true, dialog: false, fallback: true };
    return res;
  }
}

// sendToCoreForBT 请核心弹「新建磁力/种子任务」窗并预填磁链(不自动建任务):
// 核心唤窗并记入持久待办,前端取走弹窗,由用户点「创建任务」。核心未更新(旧二进制
// 不认 magnet 语义 → 400/404)或拒绝时,回退旧的直接建任务(挂起待选),保证磁链
// 仍交给 ZDownloader、下载不被打断;核心离线则报「无法连接」。
async function sendToCoreForBT(st, magnet) {
  try {
    const resp = await fetch(serverBase(st) + "/api/ui/newtask", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ magnet }),
    });
    if (resp.ok) {
      const data = await resp.json();
      if (data && data.ok === true) return { ok: true, dialog: true };
      // 核心拒绝(如空磁链)→ 落到回退
    }
  } catch (e) { /* 网络错误/旧核心 404 → 落到回退 */ }
  // 回退:直接建任务并挂起待选(兼容旧核心),磁链仍交给 ZDownloader。
  try {
    const resp = await fetch(serverBase(st) + "/api/tasks/bt", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ magnet, choose: true, chooseAny: true }),
    });
    if (!resp.ok) return { ok: false, error: "核心返回 HTTP " + resp.status };
    const data = await resp.json();
    if (data && data.errors && data.errors.length && !(data.tasks || []).length) {
      return { ok: false, error: data.errors[0] };
    }
    return { ok: true, dialog: false, fallback: true };
  } catch (e) {
    return { ok: false, error: "无法连接 ZDownloader 核心" };
  }
}

/* ---------------- 角标与通知 ---------------- */
function flashBadge(text, color = "#4f8cff") {
  chrome.action.setBadgeBackgroundColor({ color });
  chrome.action.setBadgeText({ text });
  setTimeout(() => chrome.action.setBadgeText({ text: "" }), 2500);
}
function notify(title, message) {
  chrome.notifications.create({
    type: "basic",
    iconUrl: chrome.runtime.getURL("icons/icon128.png"),
    title,
    message,
  });
}

/* ---------------- 下载捕获 ---------------- */
// 所有 http(s) 下载(无 minSizeMB 阈值)都接管,但用 Chrome「另存为」对话框里用户选定的
// 保存地址:不再弹我们自己的对话框。onCreated 时 item.filename 尚是默认值,须等写盘真正
// 接管策略:filename 非空的下载(自动目录模式)在 onCreated 立即投递;「另存为」未回答时
// filename 尚空,延迟到 onChanged 里 filename 由空变非空时投递。不再用内存 Map 记 pending
// ——MV3 Service Worker 随时会被杀,内存状态会丢,这正是之前「另存为」选完路径却不投递的
// 根因。改由 onChanged 直接 fetch 该下载拿 url,重跑同一套守卫,天然扛 Service Worker 重启。
async function passGuards(st, url) {
  if (!st.enabled) return false;
  if (!/^https?:/i.test(url)) return false;            // 仅 http(s)
  if (isOurServer(st, url)) return false;              // 忽略核心自身
  if (recentlyCaptured(url)) return false;             // 5 分钟内重复事件去重
  try {
    if (inList(st.domainBlacklist, new URL(url).hostname)) return false;
  } catch (e) {
    return false;
  }
  if (inList(st.extBlacklist, extOf(url))) return false;
  return true;
}

async function dispatchCapture(st, item, url) {
  // item.filename 此刻是用户在 Chrome「另存为」里选定的完整保存路径,拆出目录与文件名交核心。
  const headers = {};
  const cookie = await cookiesFor(url);
  if (cookie) headers.Cookie = cookie;
  const res = await sendToCore(st, {
    url,
    filename: basename(item.filename || ""),
    dir: dirname(item.filename || "") || undefined,
    referer: item.referrer || undefined,
    headers: Object.keys(headers).length ? headers : undefined,
  });
  if (res && res.ok) {
    markCaptured(url); // 成功接管才打标,防 Chrome 对同一资源重复派发风暴(不阻塞延迟链路)
    // 已成功交给核心:取消 Chrome 自己的下载,由 ZDownloader 下到用户选定的地址。
    try { await chrome.downloads.cancel(item.id); } catch (e) { /* 可能已结束 */ }
    try {
      // erase 必须带精确 id;仅当仍是本条下载时执行
      const [still] = await chrome.downloads.search({ id: item.id });
      if (still) await chrome.downloads.erase({ id: item.id });
    } catch (e) { /* 忽略 */ }
    flashBadge("Z");
    notifyCaptureOk(basename(item.filename || "") || url);
  } else {
    flashBadge("!", "#f87171");
    if (st.notify) notify("ZDownloader 未响应", "本次下载仍由浏览器完成。请确认核心程序已启动。");
  }
}

chrome.downloads.onCreated.addListener(async (item) => {
  if (!item || typeof item.id !== "number") return; // 防御:异常事件直接忽略
  if (item.state && item.state !== "in_progress") return; // 只接管进行中的下载,忽略历史条目回放
  if (item.paused) return;
  const st = await getSettings();
  const url = item.finalUrl || item.url || "";
  if (!(await passGuards(st, url))) return;
  // filename 已落定(非空,如自动目录模式)→ 立即投递;否则「另存为」未回答,filename 尚空,
  // 交由 onChanged 在 filename 由空变非空时投递(无需在此登记,扛 Service Worker 重启)。
  if (item.filename) {
    await dispatchCapture(st, item, url);
  } else {
    // 「另存为」未回答:把 url 存进 chrome.storage.session(onChanged 里再取)。用 session 存储
    // 而非内存——扛 Service Worker 重启,SW 被杀后新实例仍能读回这条 url。
    await chrome.storage.session.set({ ["zdpend_" + item.id]: url });
  }
});

// 「另存为」已回答:单参数对象含 .filename(落定完整路径)与 .id。url 从 onCreated 存的
// chrome.storage.session 取回——不依赖 chrome.downloads.fetch(本机不稳),也扛 SW 重启。
chrome.downloads.onChanged.addListener(async (a, b) => {
  const isItemFirst = !!(a && a.filename); // 首参是 item → 从 a 读;是数字 → 从 b(delta) 读
  const id = isItemFirst ? a.id : a;
  const src = isItemFirst ? a : b;
  if (typeof id !== "number") return;
  const filenameDelta = src && src.filename;
  if (!filenameDelta || !filenameDelta.current) return; // 无落定路径 → 忽略
  const chosen = filenameDelta.current;                  // 用户在「另存为」选定的完整保存路径
  const key = "zdpend_" + id;
  const { [key]: url } = await chrome.storage.session.get(key);
  if (!url) return;                    // onCreated 未登记(被去重/非接管目标/已消费)→ 不接管
  await chrome.storage.session.remove(key);
  const st = await getSettings();
  if (!(await passGuards(st, url))) return;
  await dispatchCapture(st, { id, filename: chosen }, url);
});

/* ---------------- 媒体嗅探 ---------------- */
async function addSniff(tabId, entry) {
  if (tabId < 0) return;
  const key = "sniff_" + tabId;
  const obj = await chrome.storage.session.get(key);
  const list = obj[key] || [];
  if (list.some(e => e.url === entry.url)) return;
  entry.ts = Date.now();
  list.unshift(entry);
  if (list.length > SNIFF_LIMIT) list.length = SNIFF_LIMIT;
  await chrome.storage.session.set({ [key]: list });
  chrome.action.setBadgeBackgroundColor({ tabId, color: "#22d3ee" });
  chrome.action.setBadgeText({ tabId, text: "N" });
}

chrome.webRequest.onHeadersReceived.addListener(async (details) => {
  if (details.tabId < 0) return;
  const st = await getSettings();
  if (!st.enabled || !st.captureMedia) return;

  const url = details.url;
  if (isOurServer(st, url)) return;
  if (inList(st.domainBlacklist, new URL(url).hostname)) return;

  const ct = getHeader(details.responseHeaders, "content-type");
  const len = parseInt(getHeader(details.responseHeaders, "content-length") || "0", 10) || 0;
  const isPlaylist = PLAYLIST_CT.test(ct) || PLAYLIST_EXT.test(url);
  const isMedia = MEDIA_CT.test(ct) || MEDIA_EXT.test(url);
  if (!isPlaylist && !isMedia) return;

  // 直接媒体小于阈值不嗅探;清单(m3u8)无体积概念,始终记录
  if (!isPlaylist && len > 0 && len / 1048576 < st.minSizeMB) return;

  let label;
  if (isPlaylist) {
    label = /dash|mpd/i.test(ct + url) ? "DASH" : "HLS";
  } else {
    label = extOf(url).toUpperCase() || ct.split("/").pop().toUpperCase() || "MEDIA";
  }
  await addSniff(details.tabId, {
    url,
    kind: isPlaylist ? "hls" : "http",
    label,
    size: len,
    pageUrl: details.initiator || "",
  });
}, { urls: ["<all_urls>"] }, ["responseHeaders"]);

chrome.tabs.onRemoved.addListener(async (tabId) => {
  try { await chrome.storage.session.remove("sniff_" + tabId); } catch (e) { /* 忽略 */ }
});

/* ---------------- 右键菜单 ---------------- */
chrome.runtime.onInstalled.addListener(async () => {
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({
      id: "zdl-link",
      title: "用 ZDownloader 下载链接",
      contexts: ["link"],
    });
    chrome.contextMenus.create({
      id: "zdl-media",
      title: "用 ZDownloader 下载该媒体",
      contexts: ["video", "audio"],
    });
  });
  // 写入缺失的默认设置
  const stored = await chrome.storage.local.get(null);
  const patch = {};
  for (const [k, v] of Object.entries(DEFAULTS)) {
    if (!(k in stored)) patch[k] = v;
  }
  if (Object.keys(patch).length) await chrome.storage.local.set(patch);
});

chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  const st = await getSettings();
  if (!st.enabled) return;
  const url = info.linkUrl || info.srcUrl || "";
  if (!url) return;
  // 磁力链接:弹「新建磁力/种子任务」窗并预填磁链(不自动建任务,核心唤窗、
  // 前端弹窗供用户选文件/目录后点「创建任务」);旧核心回退直接建任务。
  if (/^magnet:/i.test(url)) {
    const res = await sendToCoreForBT(st, url);
    if (res.ok) { flashBadge("Z"); notifyCaptureOk(url); }
    else { flashBadge("!", "#f87171"); if (st.notify) notify("ZDownloader 未响应", res.error || "请确认核心程序已启动。"); }
    return;
  }
  if (!/^https?:/i.test(url)) return; // 其余协议忽略
  // http(s) 链接/媒体:弹新建任务窗(自动填 URL、预填 Referer/Cookie,其余自填)
  const headers = {};
  const cookie = await cookiesFor(url);
  if (cookie) headers.Cookie = cookie;
  const res = await openCoreDialog(st, { urls: [url], referer: (tab && tab.url) || undefined, headers, stay: true });
  if (res && res.ok) {
    flashBadge("Z");
    notifyCaptureOk(url);
  } else {
    flashBadge("!", "#f87171");
    if (st.notify) notify("ZDownloader 未响应", (res && res.error) || "请确认核心程序已启动。");
  }
});

/* ---------------- popup 消息 ---------------- */
chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  (async () => {
    if (msg.type === "getSniffs") {
      const key = "sniff_" + msg.tabId;
      const obj = await chrome.storage.session.get(key);
      sendResponse({ list: obj[key] || [] });
    } else if (msg.type === "clearSniffs") {
      await chrome.storage.session.remove("sniff_" + msg.tabId);
      chrome.action.setBadgeText({ tabId: msg.tabId, text: "" });
      sendResponse({ ok: true });
    } else if (msg.type === "addTask") {
      // 快加/嗅探「下载」:弹新建任务窗(自动填 URL、预填 Referer/Cookie,其余自填),
      // 与右键一致;核心未更新时 openCoreDialog 内部回退直接建任务。
      const st = await getSettings();
      const t = msg.task || {};
      const urls = t.url ? [t.url] : [];
      const headers = {};
      if (t.url) {
        const cookie = await cookiesFor(t.url);
        if (cookie) headers.Cookie = cookie;
      }
      if (!urls.length) {
        sendResponse({ ok: false, error: "缺少链接" });
        return;
      }
      const res = await openCoreDialog(st, { urls, referer: t.referer || undefined, headers, stay: true });
      sendResponse(res);
    } else if (msg.type === "ping") {
      const st = await getSettings();
      sendResponse(await pingCore(st));
    } else if (msg.type === "stats") {
      const st = await getSettings();
      try {
        const resp = await fetch(serverBase(st) + "/api/stats");
        sendResponse(await resp.json());
      } catch (e) {
        sendResponse(null);
      }
    }
  })();
  return true; // 异步响应
});
