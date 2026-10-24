<script setup>
// 任务详情模态窗(概览/速度/文件/连接/Tracker 五页签):
// - 1s 轮询 /api/tasks/{id}/detail,客户端按秒采样累计量增量画速度曲线
// - 概览网格、限速编辑(编辑中轮询不回写)、文件改选(勾选未应用前轮询不重建表)
// - 连接/Tracker 表点击表头排序、右键菜单(断开/封锁/封禁客户端/编辑 tracker…)
// - 子弹窗:Tracker 详情/添加编辑/日志、手动添加连接(随抽屉一起关闭)
// DOM 结构逐位对齐旧 #drawerOverlay。
import { ref, reactive, computed, watch, nextTick, onMounted, onBeforeUnmount } from "vue";
import { api } from "../lib/api.js";
import { store, ui, openOverlay, closeOverlay, toast, showErr, confirmBox, refreshTasks } from "../store.js";
import { fmtBytes, fmtSpeed, fmtTime, magnetFallbackName, STATUS_TEXT } from "../lib/fmt.js";
import { fmtDur, SRC_TEXT, sortRows, selectionIn } from "../lib/peerfmt.js";
import { isPaddingFile } from "../lib/filetree.js";
import { ms, SVGS } from "../lib/icons.js";
import BitfieldCanvas from "./BitfieldCanvas.vue";
import FileTree from "./FileTree.vue";
import Overlay from "./Overlay.vue";
import ZSelect from "./ZSelect.vue";
import { showCtxMenu, hideCtxMenu } from "../lib/ctxmenu.js";

const UNIT_OPTS = [{ value: "KB", label: "KB/s" }, { value: "MB", label: "MB/s" }];

/* ---------------- 状态 ---------------- */
const activeTab = ref("overview");
const lastDetail = ref(null);
const peerSort = reactive({ key: "ip", dir: 1 });
const trSort = reactive({ key: "url", dir: 1 });
const peerRows = ref([]);
const peerConnecting = ref(false);
const trRows = ref([]);
const peerTableEl = ref(null), trTableEl = ref(null);
const dwLimit = ref(0), dwLimitUnit = ref("KB"), dwUpLimit = ref(0), dwUpLimitUnit = ref("MB");
const dwLimitDirty = ref(false);
const dwSelDirty = ref(false);
const fileTree = ref(null);
const dwFiles = ref([]), dwHasMeta = ref(false);
const fileTreeEl = ref(null);
const speedCanvas = ref(null);
const pollTick = ref(0);
let drawerTimer = null;

// 详情速度曲线:客户端按秒采样累计量增量;任务删除后残留条目超限时清理
const speedHistMap = new Map();
const SPEED_PTS_MAX = 180; // 1s 采样 × 3 分钟

function sampleSpeedHistory(id, t) {
  const now = Date.now();
  let h = speedHistMap.get(id);
  if (!h) { h = { last: null, pts: [], down: 0, up: 0 }; speedHistMap.set(id, h); }
  const cur = { dl: t.downloaded || 0, ul: t.uploaded || 0, t: now };
  if (h.last) {
    const dt = (now - h.last.t) / 1000;
    if (dt > 0.05) {
      // 累计量回退(会话重启)视为 0 速,不产生负数尖峰
      const dl = Math.max(0, cur.dl - h.last.dl) / dt;
      const ul = Math.max(0, cur.ul - h.last.ul) / dt;
      h.down = dl; h.up = ul;
      h.pts.push([dl, ul]);
      if (h.pts.length > SPEED_PTS_MAX) h.pts.shift();
    }
  }
  h.last = cur;
}

/* ---------------- 打开/关闭/轮询 ---------------- */
function openDrawer(id) {
  const t = store.tasks.find(x => x.id === id);
  if (!t) return;
  store.detailId = id;
  dwLimitDirty.value = false; // 新任务打开:输入框按服务端值重建
  dwSelDirty.value = false;
  switchTab("overview");
  // 速度历史按任务保留;任务删除后残留条目超限时清理
  if (speedHistMap.size > 60) {
    const alive = new Set(store.tasks.map(x => x.id));
    for (const k of speedHistMap.keys()) {
      if (!alive.has(k)) speedHistMap.delete(k);
    }
  }
  openOverlay("drawerOverlay");
  pollDrawer();
}

function closeDrawer() {
  closeOverlay("drawerOverlay");
}

watch(() => ui.flags.drawerOverlay, v => {
  if (v) {
    if (!drawerTimer) drawerTimer = setInterval(pollDrawer, 1000);
  } else {
    // 关闭抽屉连带收起关联弹窗与右键菜单,清轮询
    closeOverlay("trOverlay");
    closeOverlay("trAddOverlay");
    closeOverlay("peerAddOverlay");
    closeOverlay("trLogOverlay");
    hideCtxMenu();
    if (drawerTimer) { clearInterval(drawerTimer); drawerTimer = null; }
    store.detailId = null;
    lastDetail.value = null;
  }
});

async function pollDrawer() {
  if (!store.detailId) return;
  try {
    const d = await api("GET", `/api/tasks/${store.detailId}/detail`);
    lastDetail.value = d;
    sampleSpeedHistory(store.detailId, d);
    applyDrawer(d);
    pollTick.value++;
  } catch (e) { /* 任务可能刚被删除 */ }
}

/* ---------------- 概览网格 ---------------- */
const gridRows = computed(() => {
  const t = lastDetail.value;
  if (!t) return [];
  const isBT = t.kind === "bt";
  const rows = [
    ["状态", STATUS_TEXT[t.status] || t.status],
    ["类型", t.kind === "hls" ? "HLS 流媒体" : (isBT ? "BitTorrent" : "HTTP 直链")],
    ["保存目录", t.dir],
    ["大小", t.size > 0 ? `${fmtBytes(t.arrived > 0 ? t.arrived : (isBT ? Math.min(t.downloaded, t.size) : t.downloaded))} / ${fmtBytes(t.size)}` : fmtBytes(t.downloaded)],
    ["速度", t.status === "running" || t.status === "seeding" ? fmtSpeed(t.speed) : "—"],
    ["线程/连接", t.connections],
    ["代理", t.proxyOverride === "none" ? "直连(不走代理)" : (t.proxyOverride || "跟随全局设置")],
    ["创建时间", fmtTime(t.createdAt)],
  ];
  if (isBT) {
    rows.push(
      ["InfoHash", t.infoHash || "—"],
      ["piece", (t.piecesDone || 0) + " / " + (t.piecesTotal || 0) + " 已完成"],
      ["已上传", fmtBytes(t.uploaded || 0) + (t.downloaded > 0 ? "(分享率 " + ((t.uploaded || 0) / t.downloaded).toFixed(2) + ")" : "")],
      ["做种策略(本任务)", t.seedRatio > 0 || t.seedMinutes > 0
        ? (t.seedRatio > 0 ? "分享率 " + t.seedRatio : "") +
          (t.seedRatio > 0 && t.seedMinutes > 0 ? " 或 " : "") +
          (t.seedMinutes > 0 ? t.seedMinutes + " 分钟" : "")
        : "跟随全局设置"],
    );
  }
  if (t.status === "completed" && t.completedAt && !t.completedAt.startsWith("0001")) rows.push(["完成时间", fmtTime(t.completedAt)]);
  if (t.status === "error" && t.error) rows.push(["错误", t.error]);
  return rows;
});

/* ---------------- 应用一拍详情(旧 renderDrawer) ---------------- */
function unitInput(bytes) {
  // 返回 {v, u}:优先整 MB,其次整 KB,否则两位小数 MB
  if (bytes > 0 && bytes % 1048576 === 0) return { v: bytes / 1048576, u: "MB" };
  if (bytes > 0 && bytes % 1024 === 0) return { v: bytes / 1024, u: "KB" };
  return { v: bytes ? +(bytes / 1048576).toFixed(2) : 0, u: "MB" };
}

function applyDrawer(d) {
  const t = d;
  const isBT = t.kind === "bt";
  // 限速编辑(用户编辑中不回写,否则秒级轮询每拍覆写值、输入无法进行)
  if (!dwLimitDirty.value) {
    if (isBT) {
      const up = unitInput(t.upLimit || 0);
      dwUpLimit.value = up.v; dwUpLimitUnit.value = up.u;
    }
    const dl = unitInput(t.speedLimit || 0);
    dwLimit.value = dl.v; dwLimitUnit.value = dl.u;
  }
  // 非 BT 任务不可见的明细 tab:自动回概览(用户停在速度页不被拽回)
  if (!isBT && ["files", "peers", "trackers"].includes(activeTab.value)) switchTab("overview");
  // 文件(独立选项卡):choosing 任务勾选即创建参数;其余改选需应用
  if (isBT) {
    const all = d.files || [];
    dwHasMeta.value = all.length > 0;
    dwFiles.value = all.filter(f => !isPaddingFile(f));
    if (!dwSelDirty.value && fileTree.value) fileTree.value.selReset && fileTree.value.selReset();
  }
  buildPeerRows(d);
  buildTrRows(d);
}

/* ---------------- 文件页签 ---------------- */
const t = computed(() => lastDetail.value);
const isBT = computed(() => t.value?.kind === "bt");

// 边下边播:文件树播放按钮 → 全局播放面板(单例 overlay)
function playFile(path) {
  store.player = { taskId: store.detailId, file: path };
  openOverlay("playerOverlay");
}

const dwEditable = computed(() => dwFiles.value.length > 0 && t.value?.status !== "canceled");
const showApply = computed(() => dwEditable.value && dwSelDirty.value && t.value?.status !== "choosing");
const selStatC = computed(() => {
  if (!fileTree.value || !dwFiles.value.length) return null;
  return fileTree.value.selStat(dwFiles.value);
});
const applyText = computed(() => {
  if (!dwSelDirty.value) return "应用更改";
  const st = selStatC.value;
  return st ? `应用更改(已选 ${st.n}/${st.total} · ${fmtBytes(st.bytes)})` : "应用更改";
});
const chooseText = computed(() => {
  if (t.value?.status !== "choosing") return "创建下载任务…";
  const st = selStatC.value;
  if (!st) return "创建下载任务…";
  return `创建下载任务(已选 ${st.n}/${st.total} · ${fmtBytes(st.bytes)})`;
});

function onFileChange() {
  dwSelDirty.value = true;
  forceSelStatTick();
}
const selTick = ref(0);
function forceSelStatTick() { selTick.value++; }

// 全选/全不选:按当前多数派翻转
function selAllToggle() {
  if (!fileTree.value || !dwEditable.value) return;
  const all = fileTree.value.selStat(dwFiles.value).all;
  fileTree.value.setAll(!all);
}
async function applySel() {
  if (!fileTree.value || !store.detailId) return;
  const sel = fileTree.value.selectedPaths();
  const body = sel === null ? { all: true } : { files: sel };
  try {
    await api("POST", `/api/tasks/${store.detailId}/files`, body);
    dwSelDirty.value = false;
    toast("文件选择已更新", "ok");
    pollDrawer();
  } catch (err) {
    toast("应用失败: " + err.message, "err");
  }
}
// 待创建任务(磁链 choosing):勾选即创建参数,直接按当前勾选创建并开始下载
async function chooseCreate() {
  const task = store.tasks.find(x => x.id === store.detailId);
  if (!task || task.status !== "choosing") { openChooseDialog(store.detailId); return; }
  if (!fileTree.value || !dwFiles.value.length) { toast("元数据尚未就绪,稍后再试", "err"); return; }
  const sel = fileTree.value.selectedPaths();
  const body = sel === null ? { all: true } : { files: sel };
  try {
    await api("POST", `/api/tasks/${store.detailId}/files`, body);
    dwSelDirty.value = false;
    toast("下载任务已创建", "ok");
    pollDrawer();
  } catch (err) {
    toast("创建失败: " + err.message, "err");
  }
}

/* ---------------- 连接表 ---------------- */
function buildPeerRows(d) {
  const tb = peerTableEl.value;
  if (tb && selectionIn(tb)) return; // 用户正在选中复制,跳过本轮重渲染
  const peers = d.peers || [];
  peerConnecting.value = isBT.value && t.value?.status === "running" && !peers.length;
  const bannedSet = new Set(d.banned || []);
  const rows = peers.map(p => {
    const ci = String(p.addr || "").lastIndexOf(":");
    const ip = ci > 0 ? p.addr.slice(0, ci) : p.addr;
    const port = ci > 0 ? p.addr.slice(ci + 1) : "";
    const st = p.downRate > 1024 ? "ok" : (p.upRate > 1024 ? "warn" : "");
    const src = SRC_TEXT[p.source] || p.source || "—";
    // piece 有效率:该 IP 交付的块参与过的 piece 里通过校验的比例
    const okN = p.piecesOK || 0, badN = p.piecesBad || 0, soleN = p.soleBad || 0;
    const effN = okN + badN;
    const eff = effN ? okN / effN : -1;
    const effCls = badN >= 3 ? " eff-bad" : (badN > 0 ? " eff-warn" : "");
    const effTxt = effN ? (eff * 100).toFixed(eff < 1 ? 1 : 0) + "%" : "—";
    const effTip = effN
      ? `参与 ${effN} 片:通过 ${okN} / 失败 ${badN}(其中独占来源 ${soleN})`
      : "尚未参与任何 piece 校验(无有效样本)";
    return {
      key: p.addr,
      addr: p.addr, client: p.client || "", ip, port,
      banned: bannedSet.has(ip),
      st, src, srcKey: p.source || "",
      prog: (p.progress * 100).toFixed(0) + "%",
      bitfield: p.bitfield || "",
      downRate: p.downRate > 0 ? fmtSpeed(p.downRate) : "",
      upRate: p.upRate > 0 ? fmtSpeed(p.upRate) : "",
      down: fmtBytes(p.down), up: fmtBytes(p.up),
      effTxt, effCls, effTip,
      inbound: p.inbound ? "远程" : "本地",
      proto: p.utp ? "uTP" : "TCP",
      enc: p.encrypted ? "E" : "",
      connFor: fmtDur(p.connectedFor),
      flags: p.flags || "",
      listenPort: p.listenPort || "—",
      v: {
        ip, client: p.client || "", rport: +port || 0, lport: p.listenPort || 0,
        prog: p.progress || 0, src,
        down: p.downRate || 0, up: p.upRate || 0, downsz: p.down || 0, upsz: p.up || 0,
        eff, inb: p.inbound ? 1 : 0, proto: p.utp ? 1 : 0, enc: p.encrypted ? 1 : 0,
        conn: p.connectedFor || 0, flags: p.flags || "",
      },
    };
  });
  sortRows(rows, peerSort, ["ip", "rport"]);
  peerRows.value = rows;
}

/* ---------------- Tracker 表 ---------------- */
const remainMs = tr => tr.nextAnnounce ? new Date(tr.nextAnnounce) - Date.now() : Infinity;
function remainTxt(tr) {
  const ms2 = remainMs(tr);
  if (ms2 === Infinity) return "—";
  return ms2 <= 0 ? "即将通告" : fmtDur(ms2 / 1000);
}
function buildTrRows(d) {
  const tb = trTableEl.value;
  if (tb && selectionIn(tb)) return; // 选中文本保护
  const rows = [];
  // 虚拟来源行:DHT / PEX(对照 BitComet 的非 tracker 来源)
  if (d.dhtNodes) {
    rows.push({
      key: "dht", virtual: true,
      urlLabel: "🕸 DHT Network",
      stCls: "ok", remain: "—", seeds: "", leech: "", users: d.dhtNodes, retries: "",
      msgCls: "dim-cell", msg: `路由表 ${d.dhtNodes} 个节点`,
      v: { url: "DHT Network", st: 0, remain: 0, seeds: 0, leech: 0, users: 0, retries: 0, msg: "" },
    });
  }
  if (d.pexReceived) {
    rows.push({
      key: "pex", virtual: true,
      urlLabel: "🔁 Peer Exchange",
      stCls: "ok", remain: "—", seeds: "", leech: "", users: d.pexReceived, retries: "",
      msgCls: "dim-cell", msg: `PEX 累计发现 ${d.pexReceived} 个 peer`,
      v: { url: "Peer Exchange", st: 0, remain: 0, seeds: 0, leech: 0, users: 0, retries: 0, msg: "" },
    });
  }
  for (const tr of (d.trackers || [])) {
    const users = (tr.seeds || 0) + (tr.leechers || 0);
    rows.push({
      key: tr.url, virtual: false, url: tr.url,
      stCls: tr.lastErr ? "err" : (tr.lastOk ? "ok" : "warn"),
      remain: remainTxt(tr),
      seeds: tr.seeds || "", leech: tr.leechers || "", users: users || "", retries: tr.retries || "",
      msg: tr.message || "—", msgTitle: tr.lastErr || "",
      v: {
        url: tr.url || "", st: tr.lastErr ? 2 : (tr.lastOk ? 0 : 1),
        remain: remainMs(tr), seeds: tr.seeds || 0, leech: tr.leechers || 0,
        users, retries: tr.retries || 0, msg: tr.message || "",
      },
    });
  }
  sortRows(rows, trSort, ["url"]);
  trRows.value = rows;
}

/* ---------------- 页签切换 ---------------- */
function switchTab(name) {
  activeTab.value = name;
  // 明细表格 tab 下弹窗加宽,表区固定占满剩余高度(pane-table)
  const tablePane = name === "peers" || name === "trackers";
  drawerEl.value?.classList.toggle("wide", tablePane);
  drawerEl.value?.classList.toggle("pane-table", tablePane);
  if (name === "peers") requestAnimationFrame(() => {}); // 位图随行组件自绘
  if (name === "speed") requestAnimationFrame(drawTaskSpeed);
}
const drawerEl = ref(null);

/* ---------------- 速度曲线 ---------------- */
function drawTaskSpeed() {
  const cv = speedCanvas.value;
  if (!cv || !cv.offsetParent) return; // 面板未显示
  const h = speedHistMap.get(store.detailId);
  if (!h) return;
  // 按容器实际尺寸重设画布分辨率(2x 抗模糊)
  const w = cv.clientWidth, ht = cv.clientHeight;
  if (cv.width !== w * 2) { cv.width = w * 2; cv.height = ht * 2; }
  const ctx = cv.getContext("2d");
  ctx.setTransform(2, 0, 0, 2, 0, 0);
  ctx.clearRect(0, 0, w, ht);

  const pts = h.pts;
  if (pts.length < 2) {
    ctx.fillStyle = getComputedStyle(document.body).getPropertyValue("--text-faint");
    ctx.font = "13px sans-serif";
    ctx.fillText("正在采样速度…", 14, 26);
    return;
  }
  const max = Math.max(...pts.flat(), 1024) * 1.15;
  const n = pts.length;
  const line = (col, fillCol, idx) => {
    ctx.beginPath();
    for (let i = 0; i < n; i++) {
      const x = (i / (SPEED_PTS_MAX - 1)) * (w - 12) + 6;
      const y = ht - 6 - (pts[i][idx] / max) * (ht - 16);
      i ? ctx.lineTo(x, y) : ctx.moveTo(x, y);
    }
    ctx.strokeStyle = col;
    ctx.lineWidth = 2.2;
    ctx.lineJoin = "round";
    ctx.stroke();
    ctx.lineTo((n - 1) / (SPEED_PTS_MAX - 1) * (w - 12) + 6, ht);
    ctx.lineTo(6, ht);
    ctx.closePath();
    ctx.fillStyle = fillCol;
    ctx.fill();
  };
  line("#34d399", "rgba(52,211,153,0.10)", 1);  // 上传
  line("#4f8cff", "rgba(79,140,255,0.14)", 0);  // 下载(上层)
}
watch([pollTick, activeTab], () => {
  if (activeTab.value === "speed") requestAnimationFrame(drawTaskSpeed);
});
const histNow = computed(() => speedHistMap.get(store.detailId));
// 速度卡片数字随轮询刷新
const downSpeedTxt = computed(() => { void pollTick.value; const h = histNow.value; return fmtSpeed(h ? h.down : 0); });
const upSpeedTxt = computed(() => { void pollTick.value; const h = histNow.value; return fmtSpeed(h ? h.up : 0); });

/* ---------------- 动作 ---------------- */
async function saveTorrent(task) {
  // 另存为种子:后端弹原生"另存为"对话框(不走 webview 内置下载),
  // 返回用户实际保存的路径。用户在对话框点取消时后端返回 "已取消"。
  try {
    const r = await fetch(`/api/tasks/${task.id}/save-torrent`, { method: "POST" });
    const e = await r.json().catch(() => ({}));
    if (!r.ok) { toast((e && e.error) || "种子尚未就绪(元数据未获取)", "err"); return; }
    toast(e.path === "已取消" ? "已取消另存" : "已另存种子到 " + e.path, "ok");
  } catch (e) { showErr(e); }
}

function onLimitInput() { dwLimitDirty.value = true; }

async function saveLimits() {
  const task = store.tasks.find(x => x.id === store.detailId);
  const v = parseFloat(dwLimit.value) || 0;
  const body = { speedLimit: Math.round(v * (dwLimitUnit.value === "MB" ? 1048576 : 1024)) };
  if (task && task.kind === "bt") {
    const uv = parseFloat(dwUpLimit.value) || 0;
    body.upLimit = Math.round(uv * (dwUpLimitUnit.value === "MB" ? 1048576 : 1024));
  }
  try {
    await api("PUT", `/api/tasks/${store.detailId}`, body);
  } catch (e) { showErr(e); return; }
  dwLimitDirty.value = false; // 已落库:下拍轮询按服务端值回填
  toast("任务属性已保存,限速立即生效", "ok");
  refreshTasks();
}

async function reannounce() {
  try {
    await api("POST", `/api/tasks/${store.detailId}/trackers`, { reannounce: true });
    toast("已请求立即重新通告", "ok");
    pollDrawer();
  } catch (e) { showErr(e); }
}

// Tracker 行右键操作
function trackerAction(body, okMsg) {
  return api("POST", `/api/tasks/${store.detailId}/trackers`, body)
    .then(() => { toast(okMsg, "ok"); pollDrawer(); })
    .catch(e => showErr(e));
}
function peerAction(body, okMsg) {
  return api("POST", `/api/tasks/${store.detailId}/peers`, body)
    .then(() => { toast(okMsg, "ok"); pollDrawer(); })
    .catch(e => showErr(e));
}

/* ---------------- Tracker 添加/编辑对话框 ---------------- */
const trAddText = ref("");
let trEditTarget = null;
const isTrackerURL = s => /^https?:\/\//.test(s) || /^udp:\/\//.test(s);
function openTrackerAdd() {
  trEditTarget = null;
  trAddTitle.value = "添加 Tracker";
  trAddHint.value = "每行一个地址,支持 http(s):// 与 udp://,空行自动忽略";
  trAddSubmitLabel.value = "添加";
  trAddText.value = "";
  openOverlay("trAddOverlay");
  nextTick(() => trAddTextEl.value?.focus());
}
function openTrackerEdit(url) {
  trEditTarget = url;
  trAddTitle.value = "编辑 Tracker";
  trAddHint.value = "修改地址后点保存(仅支持 http(s):// 与 udp://)";
  trAddSubmitLabel.value = "保存";
  trAddText.value = url;
  openOverlay("trAddOverlay");
  nextTick(() => trAddTextEl.value?.focus());
}
const trAddTitle = ref("添加 Tracker"), trAddHint = ref(""), trAddSubmitLabel = ref("添加");
const trAddTextEl = ref(null);

async function trAddSubmit() {
  try {
    if (trEditTarget !== null) {
      const nu = trAddText.value.split(/\r?\n/).map(s => s.trim()).filter(Boolean)[0] || "";
      if (!isTrackerURL(nu)) { toast("地址需以 http(s):// 或 udp:// 开头", "warn"); return; }
      if (nu !== trEditTarget) {
        await api("POST", `/api/tasks/${store.detailId}/trackers`, { remove: trEditTarget });
        await api("POST", `/api/tasks/${store.detailId}/trackers`, { add: nu });
      }
      trEditTarget = null;
      closeOverlay("trAddOverlay");
      toast("tracker 已更新", "ok");
      pollDrawer();
      return;
    }
    const all = trAddText.value.split(/\r?\n/).map(s => s.trim()).filter(Boolean);
    const lines = all.filter(isTrackerURL);
    const badN = all.length - lines.length;
    if (!lines.length) { toast("没有可添加的地址,每行需以 http(s):// 或 udp:// 开头"); return; }
    const r = await api("POST", `/api/tasks/${store.detailId}/trackers`, { adds: lines });
    closeOverlay("trAddOverlay");
    const failed = r.failed || [];
    const parts = [`已添加 ${r.added} 个 tracker 并开始通告`];
    if (r.skipped) parts.push(`${r.skipped} 个已存在(同服务器)跳过`);
    if (failed.length) parts.push(`${failed.length} 个添加失败`);
    if (badN) parts.push(`${badN} 行格式无效已忽略`);
    toast(parts.join(","), failed.length || badN ? "warn" : "ok");
    pollDrawer();
  } catch (e) { showErr(e); }
}

function removeInvalidTrackers() {
  const bad = (lastDetail.value?.trackers || []).filter(x => x.lastErr);
  if (!bad.length) { toast("没有通告失败的 tracker", "ok"); return; }
  confirmBox(`将删除 ${bad.length} 个通告失败的 tracker,确定?`, {
    onOk: async () => {
      let n = 0;
      for (const x of bad) {
        try { await api("POST", `/api/tasks/${store.detailId}/trackers`, { remove: x.url }); n++; } catch { /* 单个失败继续 */ }
      }
      toast(`已删除 ${n} 个无效 tracker`, "ok");
      pollDrawer();
    },
  });
}
// tracker 去重:同 host:port(如 http://x:8000 与 udp://x:8000)保留一个
function dedupeTrackers() {
  api("POST", `/api/tasks/${store.detailId}/trackers`, { dedupe: true })
    .then(r => {
      const n = (r.removed || []).length;
      toast(n > 0 ? `已去除 ${n} 个重复 tracker(同服务器保留通告成功的一个)` : "没有重复的 tracker", n > 0 ? "ok" : "");
      pollDrawer();
    })
    .catch(e => showErr(e));
}
function showTrackerLog(url) {
  const tr = (lastDetail.value?.trackers || []).find(x => x.url === url);
  trLogUrl.value = url;
  const lines = [];
  if (!tr) {
    lines.push("(无状态记录)");
  } else {
    lines.push(`最近结果: ${tr.message || "—"}`);
    if (tr.lastErr) lines.push(`最近错误: ${tr.lastErr}`);
    lines.push(`连续失败: ${tr.retries || 0} 次`);
    lines.push(`最近成功: ${tr.lastOk ? fmtTime(tr.lastOk) : "从未"}`);
    lines.push(`下次通告: ${tr.nextAnnounce ? fmtTime(tr.nextAnnounce) : "—"}`);
  }
  trLogBody.value = lines.join("\n");
  openOverlay("trLogOverlay");
}
const trLogUrl = ref(""), trLogBody = ref("");

/* ---------------- 连接右键 / 手动添加 ---------------- */
function openPeerAdd() {
  if (!store.detailId) return;
  peerAddText.value = "";
  openOverlay("peerAddOverlay");
  nextTick(() => peerAddTextEl.value?.focus());
}
const peerAddText = ref(""), peerAddTextEl = ref(null);
async function peerAddSubmit() {
  const lines = peerAddText.value.split(/\r?\n/).map(s => s.trim()).filter(Boolean);
  if (!lines.length) { toast("请输入至少一个 peer 地址(ip:port)"); return; }
  try {
    const r = await api("POST", `/api/tasks/${store.detailId}/peers`, { add: lines });
    closeOverlay("peerAddOverlay");
    toast(`已提交 ${r.added ?? lines.length} 个连接请求,握手成功即出现在列表`, "ok");
    pollDrawer();
  } catch (e) { showErr(e); }
}

function onPeerCtx(e) {
  e.preventDefault();
  const tr = e.target.closest("tr[data-addr]");
  if (!tr) {
    showCtxMenu(e.clientX, e.clientY, [
      { label: "手动添加连接…", action: openPeerAdd },
    ]);
    return;
  }
  const addr = tr.dataset.addr;
  const ip = addr.slice(0, addr.lastIndexOf(":"));
  const client = tr.dataset.client || "";
  const items = [
    { label: "断开连接", action: () => peerAction({ remove: addr }, "连接已断开") },
    { label: "封锁此 IP…", action: () => confirmBox(
      `将封锁 ${ip} 并断开其所有连接,本任务内不再与其建立连接,确定?`,
      { onOk: () => peerAction({ ban: addr }, `已封锁 ${ip}`) }) },
  ];
  // 按客户端名全局封禁(该名的全部 peer 连同其 IP 入全局封禁名单)
  if (client && client !== "未知") {
    items.push("-", { label: `封禁所有「${client}」…`, action: () => confirmBox(
      `将封禁全部任务中客户端名为「${client}」的 peer 并断开连接,并把「${client}」加入客户端黑名单(之后任意 IP 出现该客户端会在握手期被自动封禁),确定?`,
      { onOk: async () => {
          const r = await api("POST", "/api/bt/ban-client", { client });
          toast(`已封禁 ${r.banned} 个 IP,「${client}」已列入客户端黑名单`);
        } }) });
  }
  items.push(
    "-",
    { label: "复制地址", action: () => navigator.clipboard && navigator.clipboard.writeText(addr) },
    "-",
    { label: "手动添加连接…", action: openPeerAdd },
  );
  showCtxMenu(e.clientX, e.clientY, items);
}
function onTrCtx(e) {
  const tr = e.target.closest("tr[data-url]");
  if (!tr) return;
  e.preventDefault();
  const url = tr.dataset.url;
  showCtxMenu(e.clientX, e.clientY, [
    { label: "编辑 Tracker…", action: () => openTrackerEdit(url) },
    { label: "更新 Tracker", action: () => trackerAction({ reannounce: true }, "已请求立即重新通告") },
    "-",
    { label: "删除 Tracker", action: () => trackerAction({ remove: url }, "tracker 已删除") },
    { label: "查看日志", action: () => showTrackerLog(url) },
    "-",
    { label: "删除所有无效 Tracker…", action: removeInvalidTrackers },
    { label: "去除重复 Tracker", action: dedupeTrackers },
  ]);
}

/* ---------------- Tracker 详情弹窗 ---------------- */
// 独立记录任务 ID:侧栏关闭会清空 detailId,弹窗内操作不能依赖它
let trTaskId = null;
const trStat = ref(""), trListRows = ref([]);
const trModalEl = ref(null);
function renderTrModal(d) {
  const task = store.tasks.find(x => x.id === trTaskId) || {};
  const taskLevel = new Set(task.trackers || []);
  const list = d.trackers || [];
  const st = d.trackerStat;
  trStat.value = st ? `共 ${st.total} · 成功 ${st.ok} · 发现 peer ${st.peers}` : "";
  trListRows.value = list.map(tr => ({
    url: tr.url,
    err: tr.lastErr || "",
    sub: tr.lastErr ? tr.lastErr
      : (tr.lastOk ? "上次成功 " + fmtTime(tr.lastOk) : "未收到应答") + (tr.peers > 0 ? " · swarm " + tr.peers : ""),
    taskLevel: taskLevel.has(tr.url),
  }));
  if (!list.length) trListRows.value = [];
}
async function openTrModal() {
  trTaskId = store.detailId;
  if (!trTaskId) return;
  try {
    renderTrModal(await api("GET", `/api/tasks/${trTaskId}/detail`));
    openOverlay("trOverlay");
  } catch (e) { showErr(e); }
}
async function trRefresh() {
  if (!trTaskId) { toast("任务详情已关闭", "err"); return; }
  try { renderTrModal(await api("GET", `/api/tasks/${trTaskId}/detail`)); } catch (e) { showErr(e); }
}
async function trRemove(url) {
  confirmBox(`移除 tracker?\n${url}`, {
    okLabel: "移除",
    onOk: async () => {
      try {
        await api("POST", `/api/tasks/${trTaskId}/trackers`, { remove: url });
        toast("tracker 已移除", "ok");
        renderTrModal(await api("GET", `/api/tasks/${trTaskId}/detail`));
        pollDrawer();
      } catch (err) { showErr(err); }
    },
  });
}

/* ---------------- 排序 ---------------- */
function thCls(sort, key) {
  return {
    sortable: true,
    "sort-asc": sort.key === key && sort.dir === 1,
    "sort-desc": sort.key === key && sort.dir === -1,
  };
}
function toggleSort(sort, key) {
  if (sort.key === key) sort.dir = -sort.dir;
  else { sort.key = key; sort.dir = 1; }
}

onMounted(() => {
  store.actions.openDrawer = openDrawer;
  store.actions.closeDrawer = closeDrawer;
});
onBeforeUnmount(() => {
  if (store.actions.openDrawer === openDrawer) delete store.actions.openDrawer;
  if (store.actions.closeDrawer === closeDrawer) delete store.actions.closeDrawer;
  if (drawerTimer) clearInterval(drawerTimer);
});
defineExpose({ closeDrawer });
</script>

<template>
  <Overlay id="drawerOverlay" @mask-close="closeDrawer">
    <div class="dialog detail-dialog" id="drawer" ref="drawerEl">
      <div class="drawer-head">
        <span class="accent-bar"></span>
        <div class="drawer-title">
          <div class="name" :title="t ? (t.filename || magnetFallbackName(t) || '(探测后确定)') : ''">{{ t ? (t.filename || magnetFallbackName(t) || "(探测后确定)") : "" }}</div>
          <div class="sub" :title="t?.url">{{ t?.url }}</div>
        </div>
        <span class="status-chip" :class="t?.status" v-if="t">{{ STATUS_TEXT[t.status] || t.status }}</span>
        <button class="icon-btn" id="drawerClose" title="关闭" @click="closeDrawer">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><path d="M18 6L6 18M6 6l12 12"/></svg>
        </button>
      </div>
      <div class="dw-tabs" id="dwTabs">
        <button class="dw-tab" data-tab="overview" :class="{ active: activeTab === 'overview' }" @click="switchTab('overview')">概览</button>
        <button class="dw-tab" data-tab="speed" :class="{ active: activeTab === 'speed' }" @click="switchTab('speed')">速度</button>
        <button class="dw-tab" data-tab="files" :class="{ active: activeTab === 'files' }" v-show="isBT" @click="switchTab('files')">文件</button>
        <button class="dw-tab" data-tab="peers" id="dwTabPeers" :class="{ active: activeTab === 'peers' }" v-show="isBT" @click="switchTab('peers')">连接 <span class="dim" v-if="peerRows.length">({{ peerRows.length }})</span></button>
        <button class="dw-tab" data-tab="trackers" id="dwTabTrackers" :class="{ active: activeTab === 'trackers' }" v-show="isBT" @click="switchTab('trackers')">Tracker <span class="dim" v-if="trRows.filter(r => !r.virtual).length">({{ trRows.filter(r => !r.virtual).length }})</span></button>
      </div>

      <div class="drawer-body">
        <div class="dw-pane" v-show="activeTab === 'overview'">
          <section>
            <div class="sec-title">概要</div>
            <div class="kv-grid" id="dwGrid">
              <template v-for="r in gridRows" :key="r[0]">
                <div class="k">{{ r[0] }}</div><div class="v">{{ r[1] }}</div>
              </template>
            </div>
            <div style="display:flex;justify-content:flex-end;margin-top:8px">
              <button class="btn btn-ghost" id="dwSaveTorrent" v-show="isBT" style="padding:4px 12px;font-size:12px" @click="saveTorrent(t)">另存为种子…</button>
            </div>
          </section>
        </div>

        <div class="dw-pane" v-show="activeTab === 'speed'">
          <div class="speed-cards">
            <div class="speed-card">
              <div class="v">{{ downSpeedTxt }}</div>
              <div class="l">下载速度</div>
            </div>
            <div class="speed-card">
              <div class="v">{{ upSpeedTxt }}</div>
              <div class="l">上传速度</div>
            </div>
          </div>
          <div class="speed-chart-wrap">
            <canvas id="dwSpeedChart" ref="speedCanvas" width="1080" height="300"></canvas>
          </div>
          <div class="chart-legend">
            <span class="lg"><i class="dot down"></i>下载</span>
            <span class="lg"><i class="dot up"></i>上传</span>
            <span class="dim" style="margin-left:auto">最近 3 分钟(每秒采样)</span>
          </div>
          <div class="pane-sep"></div>
          <div class="field" id="dwLimitField" v-show="t && t.status !== 'completed' && t.status !== 'canceled'">
            <label>每任务下载限速(0 不限,保存后立即生效)</label>
            <div class="unit-row">
              <input type="number" v-model="dwLimit" min="0" step="any" @input="onLimitInput" @change="onLimitInput">
              <ZSelect v-model="dwLimitUnit" :options="UNIT_OPTS" @change="onLimitInput" />
            </div>
          </div>
          <div class="field" id="dwUpLimitField" v-show="isBT && t && t.status !== 'canceled'">
            <label>每任务上传限速(0 不限,BT 任务)</label>
            <div class="unit-row">
              <input type="number" v-model="dwUpLimit" min="0" step="any" @input="onLimitInput" @change="onLimitInput">
              <ZSelect v-model="dwUpLimitUnit" :options="UNIT_OPTS" @change="onLimitInput" />
            </div>
          </div>
          <div style="display:flex;gap:8px;justify-content:flex-end">
            <button class="btn btn-ghost" id="dwRefresh" title="立即刷新 tracker/连接信息" @click="dwLimitDirty = false; pollDrawer(); toast('已刷新')">刷新</button>
            <button class="btn btn-primary" id="dwSave" v-show="t && t.status !== 'completed' && t.status !== 'canceled'" @click="saveLimits">保存限速</button>
          </div>
        </div>

        <div class="dw-pane" v-show="activeTab === 'files'">
          <div class="sec-title" style="margin-top:0;display:flex;align-items:center">
            文件
            <button class="btn btn-ghost" id="dwSelAll" v-show="dwEditable" style="margin-left:auto;padding:4px 12px;font-size:12px" @click="selAllToggle">全选/全不选</button>
            <button class="btn" id="dwApplySel" v-show="showApply" style="padding:4px 12px;font-size:12px" @click="applySel">{{ applyText }}</button>
            <button class="btn btn-ghost" id="dwChooseBtn" v-show="t && t.status === 'choosing'" style="margin-left:8px;padding:4px 12px;font-size:12px" @click="chooseCreate">{{ chooseText }}</button>
          </div>
          <div id="dwFileList" class="file-list" ref="fileTreeEl">
            <div v-if="!dwHasMeta" class="dim" style="padding:6px 0">{{ t && t.status === 'choosing' ? "正在获取元数据,就绪后弹出创建任务窗口" : "元数据未就绪" }}</div>
            <FileTree v-else ref="fileTree" :files="dwFiles" mode="view" :editable="dwEditable"
              :task-status="t?.status" :init-id="store.detailId || ''" @change="onFileChange" @play="playFile" />
          </div>
        </div>

        <div class="dw-pane" v-show="activeTab === 'peers'">
          <div class="table-scroll">
            <table class="bt-table" id="dwPeerTable" ref="peerTableEl">
              <thead><tr>
                <th></th>
                <th :class="thCls(peerSort, 'ip')" data-sk="ip" @click="toggleSort(peerSort, 'ip')">IP</th>
                <th :class="thCls(peerSort, 'client')" data-sk="client" @click="toggleSort(peerSort, 'client')">客户端</th>
                <th :class="thCls(peerSort, 'rport')" data-sk="rport" @click="toggleSort(peerSort, 'rport')">远程端口</th>
                <th :class="thCls(peerSort, 'lport')" data-sk="lport" @click="toggleSort(peerSort, 'lport')">监听端口</th>
                <th :class="thCls(peerSort, 'prog')" data-sk="prog" @click="toggleSort(peerSort, 'prog')">进度</th>
                <th>分块图</th>
                <th :class="thCls(peerSort, 'src')" data-sk="src" @click="toggleSort(peerSort, 'src')">来源</th>
                <th :class="thCls(peerSort, 'down')" data-sk="down" @click="toggleSort(peerSort, 'down')">下载速度</th>
                <th :class="thCls(peerSort, 'up')" data-sk="up" @click="toggleSort(peerSort, 'up')">上传速度</th>
                <th :class="thCls(peerSort, 'downsz')" data-sk="downsz" @click="toggleSort(peerSort, 'downsz')">下载大小</th>
                <th :class="thCls(peerSort, 'upsz')" data-sk="upsz" @click="toggleSort(peerSort, 'upsz')">上传大小</th>
                <th :class="thCls(peerSort, 'eff')" data-sk="eff" @click="toggleSort(peerSort, 'eff')" title="piece 有效率:该 IP 交付的块参与过的 piece 中通过校验的比例(悬停看 通过/失败/独占来源 明细)">piece有效率</th>
                <th :class="thCls(peerSort, 'inb')" data-sk="inb" @click="toggleSort(peerSort, 'inb')">发起</th>
                <th :class="thCls(peerSort, 'proto')" data-sk="proto" @click="toggleSort(peerSort, 'proto')">协议</th>
                <th :class="thCls(peerSort, 'enc')" data-sk="enc" @click="toggleSort(peerSort, 'enc')">加密</th>
                <th :class="thCls(peerSort, 'conn')" data-sk="conn" @click="toggleSort(peerSort, 'conn')">连接时间</th>
                <th :class="thCls(peerSort, 'flags')" data-sk="flags" @click="toggleSort(peerSort, 'flags')">标志</th>
              </tr></thead>
              <tbody id="dwPeerRows" @contextmenu="onPeerCtx">
                <tr v-if="!peerRows.length"><td colspan="18" class="dim-cell" style="text-align:center;padding:14px 0">{{ peerConnecting ? "正在建立连接…" : "无连接" }}</td></tr>
                <tr v-for="p in peerRows" :key="p.key" :data-addr="p.addr" :data-client="p.client">
                  <td><span class="st-dot" :class="p.st"></span></td>
                  <td :title="p.client" style="font-family:var(--mono)">{{ p.ip }}<span v-if="p.banned" class="src-badge banned"> 已封锁</span></td>
                  <td class="dim-cell">{{ p.client }}</td>
                  <td class="num">{{ p.port }}</td>
                  <td class="num">{{ p.listenPort }}</td>
                  <td class="num">{{ p.prog }}</td>
                  <td><BitfieldCanvas :bf="p.bitfield" /></td>
                  <td><span class="src-badge" :class="p.srcKey">{{ p.src }}</span></td>
                  <td class="num">{{ p.downRate }}</td>
                  <td class="num">{{ p.upRate }}</td>
                  <td class="num">{{ p.down }}</td>
                  <td class="num">{{ p.up }}</td>
                  <td class="num" :class="p.effCls" :title="p.effTip">{{ p.effTxt }}</td>
                  <td class="dim-cell">{{ p.inbound }}</td>
                  <td>{{ p.proto }}</td>
                  <td>{{ p.enc }}</td>
                  <td class="num">{{ p.connFor }}</td>
                  <td class="dim-cell" title="D=在从我们下载 I=想要数据 C=choke着我">{{ p.flags }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>

        <div class="dw-pane" v-show="activeTab === 'trackers'">
          <div style="display:flex;gap:8px;margin:8px 0">
            <button class="btn btn-ghost" id="dwTrackerAddOpen" @click="openTrackerAdd">添加 Tracker…</button>
            <button class="btn btn-ghost" id="dwReannounce" @click="reannounce">重新通告</button>
          </div>
          <div class="table-scroll">
            <table class="bt-table" id="dwTrTable" ref="trTableEl">
              <thead><tr>
                <th :class="thCls(trSort, 'url')" data-sk="url" @click="toggleSort(trSort, 'url')">Tracker / 来源</th>
                <th :class="thCls(trSort, 'st')" data-sk="st" @click="toggleSort(trSort, 'st')">状态</th>
                <th :class="thCls(trSort, 'remain')" data-sk="remain" @click="toggleSort(trSort, 'remain')">剩余时间</th>
                <th :class="thCls(trSort, 'seeds')" data-sk="seeds" @click="toggleSort(trSort, 'seeds')">种子</th>
                <th :class="thCls(trSort, 'leech')" data-sk="leech" @click="toggleSort(trSort, 'leech')">下载中</th>
                <th :class="thCls(trSort, 'users')" data-sk="users" @click="toggleSort(trSort, 'users')">全部用户</th>
                <th :class="thCls(trSort, 'retries')" data-sk="retries" @click="toggleSort(trSort, 'retries')">重试</th>
                <th :class="thCls(trSort, 'msg')" data-sk="msg" @click="toggleSort(trSort, 'msg')">最近消息</th>
              </tr></thead>
              <tbody id="dwTrRows" @contextmenu="onTrCtx">
                <tr v-if="!trRows.length"><td colspan="8" class="dim-cell" style="text-align:center;padding:14px 0">无 tracker</td></tr>
                <tr v-for="r in trRows" :key="r.key" :data-url="r.virtual ? undefined : r.url">
                  <td :title="r.virtual ? undefined : r.url" style="max-width:260px;overflow:hidden;text-overflow:ellipsis">{{ r.urlLabel || r.url }}</td>
                  <td><span class="st-dot" :class="r.stCls"></span></td>
                  <td class="num">{{ r.remain }}</td>
                  <td class="num">{{ r.seeds }}</td>
                  <td class="num">{{ r.leech }}</td>
                  <td class="num">{{ r.users }}</td>
                  <td class="num">{{ r.retries }}</td>
                  <td class="dim-cell" :title="r.msgTitle" style="max-width:220px;overflow:hidden;text-overflow:ellipsis">{{ r.msg }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>
    </div>

    <!-- Tracker 详情 -->
    <Overlay id="trOverlay">
      <div class="dialog" style="width:min(760px,calc(100vw - 48px))">
        <h2><span class="accent-bar"></span>Tracker 详情 <span class="dim" style="font-size:13px;font-weight:400;margin-left:8px">{{ trStat }}</span></h2>
        <div class="hint-text">仅在打开或点击刷新时更新,不跟随实时轮询</div>
        <div id="trList" class="mini-table" ref="trModalEl" style="max-height:58vh;overflow:auto">
          <template v-if="trListRows.length">
            <div v-for="r in trListRows" :key="r.url" class="mini-row">
              <div class="mini-main">
                <div class="u" :title="r.url">{{ r.url }}</div>
                <div class="s"><span v-if="r.err" class="bad">{{ r.err }}</span>{{ r.err ? "" : r.sub }}</div>
              </div>
              <button v-if="r.taskLevel" class="icon-btn" title="移除" @click="trRemove(r.url)" v-html="SVGS.trash"></button>
            </div>
          </template>
          <div v-else class="dim" style="padding:6px 0">运行会话未启动或无 tracker</div>
        </div>
        <div class="dialog-actions">
          <button class="btn btn-ghost" id="trRefresh" @click="trRefresh">刷新</button>
          <button class="btn btn-primary" @click="closeOverlay('trOverlay')">关闭</button>
        </div>
      </div>
    </Overlay>

    <!-- 添加/编辑 Tracker -->
    <Overlay id="trAddOverlay">
      <div class="dialog" style="width:min(560px,calc(100vw - 48px))">
        <h2><span class="accent-bar"></span><span>{{ trAddTitle }}</span></h2>
        <div class="hint-text">{{ trAddHint }}</div>
        <div class="field">
          <textarea id="trAddText" ref="trAddTextEl" v-model="trAddText" rows="9" style="width:100%;resize:vertical;font-family:var(--mono)" placeholder="http://tracker.example.com/announce&#10;udp://tracker.example.com:6969/announce"></textarea>
        </div>
        <div class="dialog-actions">
          <button class="btn btn-ghost" @click="closeOverlay('trAddOverlay')">取消</button>
          <button class="btn btn-primary" id="trAddSubmit" @click="trAddSubmit">{{ trAddSubmitLabel }}</button>
        </div>
      </div>
    </Overlay>

    <!-- 手动添加连接 -->
    <Overlay id="peerAddOverlay">
      <div class="dialog" style="width:min(560px,calc(100vw - 48px))">
        <h2><span class="accent-bar"></span>手动添加连接</h2>
        <div class="hint-text">每行一个 peer 地址(ip:port),可从其他 BT 客户端的连接列表直接复制粘贴;客户端会立即尝试 BT 握手,握手成功即进入连接列表</div>
        <div class="field">
          <textarea id="peerAddText" ref="peerAddTextEl" v-model="peerAddText" rows="9" style="width:100%;resize:vertical;font-family:var(--mono)" placeholder="192.168.1.100:24521&#10;[2001:db8::1]:24521"></textarea>
        </div>
        <div class="dialog-actions">
          <button class="btn btn-ghost" @click="closeOverlay('peerAddOverlay')">取消</button>
          <button class="btn btn-primary" id="peerAddSubmit" @click="peerAddSubmit">连接</button>
        </div>
      </div>
    </Overlay>

    <!-- Tracker 日志 -->
    <Overlay id="trLogOverlay">
      <div class="dialog" style="width:min(620px,calc(100vw - 48px))">
        <h2><span class="accent-bar"></span>Tracker 日志 <span class="dim" style="font-size:12px;font-weight:400;margin-left:6px;word-break:break-all">{{ trLogUrl }}</span></h2>
        <pre id="trLogBody" class="log-view" style="max-height:46vh">{{ trLogBody }}</pre>
        <div class="dialog-actions">
          <button class="btn btn-primary" @click="closeOverlay('trLogOverlay')">关闭</button>
        </div>
      </div>
    </Overlay>
  </Overlay>
</template>
