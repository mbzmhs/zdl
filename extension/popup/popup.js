/* ZDownloader popup */
"use strict";

const $ = s => document.querySelector(s);

function fmtBytes(n) {
  if (!n || n <= 0) return "";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0, v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(v >= 100 ? 0 : v >= 10 ? 1 : 2)) + " " + u[i];
}
function fmtSpeed(n) {
  return n > 0 ? fmtBytes(n) + "/s" : "";
}

function send(msg) {
  return new Promise(resolve => {
    try {
      chrome.runtime.sendMessage(msg, r => {
        if (chrome.runtime.lastError) {
          // 后台脚本未响应/出错时不能吞掉错误,否则失败会被静默
          resolve({ ok: false, error: chrome.runtime.lastError.message || "扩展后台未响应" });
        } else {
          resolve(r);
        }
      });
    } catch (e) {
      resolve({ ok: false, error: String(e) });
    }
  });
}

function nameOf(entry) {
  try {
    const u = new URL(entry.url);
    const base = decodeURIComponent(u.pathname.split("/").pop() || u.hostname);
    return base.length > 60 ? base.slice(0, 60) + "…" : base;
  } catch (e) {
    return entry.url.slice(0, 60);
  }
}

async function init() {
  const { enabled = true } = await chrome.storage.local.get("enabled");
  $("#masterToggle").checked = enabled;
  $("#masterToggle").addEventListener("change", async e => {
    await chrome.storage.local.set({ enabled: e.target.checked });
    if (!e.target.checked) await chrome.action.setBadgeText({ text: "" });
  });

  // 核心状态与总速度
  const ping = await send({ type: "ping" });
  setCoreState(!!ping, ping ? "核心运行中 v" + (ping.version || "?") : "核心未启动(浏览器下载不受影响)");
  if (ping) {
    const stats = await send({ type: "stats" });
    if (stats) $("#totalSpeed").textContent = fmtSpeed(stats.totalSpeed);
  }

  // 嗅探列表
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (tab) {
    renderSniffs(tab.id, tab.url);
    $("#clearSniffs").addEventListener("click", async () => {
      await send({ type: "clearSniffs", tabId: tab.id });
      renderSniffs(tab.id, tab.url);
    });
  }

  // 快速添加
  $("#quickUrl").addEventListener("keydown", async e => {
    if (e.key !== "Enter") return;
    const url = e.target.value.trim();
    if (!url) return;
    const res = await send({ type: "addTask", task: { url, referer: tab ? tab.url : undefined } });
    e.target.value = "";
    setCoreState(!!res.ok, res.ok ? "已添加到下载队列" : res.error);
  });

  $("#openUI").addEventListener("click", async () => {
    const { server } = await chrome.storage.local.get("server");
    const base = (server || "http://127.0.0.1:8620").replace(/\/+$/, "");
    // 桌面版:唤起原生窗口;网页版或核心离线:回退为在浏览器打开
    try {
      const r = await fetch(base + "/api/ui/show", { method: "POST" });
      const d = await r.json();
      if (d && d.ok) { window.close(); return; }
    } catch (e) { /* 走浏览器回退 */ }
    chrome.tabs.create({ url: base + "/" });
    window.close();
  });
  $("#openOptions").addEventListener("click", () => chrome.runtime.openOptionsPage());
}

function setCoreState(on, text) {
  const dot = $("#coreState");
  dot.className = "core-dot " + (on ? "on" : "off");
  $("#coreText").textContent = text;
}

async function renderSniffs(tabId, pageUrl) {
  const { list = [] } = await send({ type: "getSniffs", tabId });
  const wrap = $("#sniffList");
  wrap.innerHTML = "";
  $("#sniffEmpty").style.display = list.length ? "none" : "";
  for (const entry of list) {
    const row = document.createElement("div");
    row.className = "sniff-item";
    const tag = document.createElement("span");
    tag.className = "tag" + (entry.kind === "hls" ? "" : " file");
    tag.textContent = entry.label || "FILE";
    const info = document.createElement("div");
    info.className = "info";
    const name = document.createElement("div");
    name.className = "name";
    name.textContent = nameOf(entry);
    name.title = entry.url;
    const meta = document.createElement("div");
    meta.className = "meta";
    const size = fmtBytes(entry.size);
    meta.textContent = size ? size : new URL(entry.url).hostname;
    info.append(name, meta);
    const btn = document.createElement("button");
    btn.className = "dl";
    btn.textContent = "下载";
    btn.addEventListener("click", async () => {
      btn.disabled = true;
      btn.textContent = "已添加";
      const task = { url: entry.url, kind: entry.kind, referer: pageUrl };
      if (entry.size > 0) task.size = entry.size;
      const res = await send({ type: "addTask", task });
      if (!res || !res.ok) {
        btn.disabled = false;
        btn.textContent = "下载";
        setCoreState(false, res && res.error ? res.error : "核心未启动");
      } else {
        setCoreState(true, "已加入下载队列");
      }
    });
    row.append(tag, info, btn);
    wrap.appendChild(row);
  }
}

init();
