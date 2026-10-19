/* ZDownloader 助手 options 页 */
"use strict";

const $ = s => document.querySelector(s);
const KEYS = ["enabled", "server", "minSizeMB", "extBlacklist", "domainBlacklist", "captureMedia", "notify", "notifySuccess"];

async function load() {
  const st = await chrome.storage.local.get(KEYS);
  $("#server").value = st.server || "http://127.0.0.1:8620";
  $("#minSizeMB").value = st.minSizeMB ?? 1;
  $("#extBlacklist").value = st.extBlacklist || "";
  $("#domainBlacklist").value = st.domainBlacklist || "";
  $("#enabled").checked = st.enabled !== false;
  $("#captureMedia").checked = st.captureMedia !== false;
  $("#notify").checked = st.notify !== false;
  $("#notifySuccess").checked = st.notifySuccess === true;
}

async function save() {
  await chrome.storage.local.set({
    server: $("#server").value.trim() || "http://127.0.0.1:8620",
    minSizeMB: Math.max(0, parseInt($("#minSizeMB").value, 10) || 0),
    extBlacklist: $("#extBlacklist").value.trim(),
    domainBlacklist: $("#domainBlacklist").value.trim(),
    enabled: $("#enabled").checked,
    captureMedia: $("#captureMedia").checked,
    notify: $("#notify").checked,
    notifySuccess: $("#notifySuccess").checked,
  });
  const saved = $("#saved");
  saved.classList.add("show");
  setTimeout(() => saved.classList.remove("show"), 1600);
}

async function test() {
  const out = $("#testResult");
  out.textContent = "连接中…";
  try {
    const base = ($("#server").value.trim() || "http://127.0.0.1:8620").replace(/\/+$/, "");
    const resp = await fetch(base + "/api/ping");
    if (!resp.ok) throw new Error("HTTP " + resp.status);
    const data = await resp.json();
    out.textContent = `✓ 连接成功:${data.name} v${data.version}`;
    out.style.color = "var(--ok)";
  } catch (e) {
    out.textContent = "✗ 连接失败:" + (e.message || e) + "(请确认核心程序已启动)";
    out.style.color = "#c0392b";
  }
}

$("#save").addEventListener("click", save);
$("#testBtn").addEventListener("click", test);
document.addEventListener("DOMContentLoaded", load);
load();
