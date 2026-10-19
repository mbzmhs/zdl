// 全局状态与数据流:替代旧 app.js 的顶层可变状态。
// 数据面契约(与后端):REST + SSE /api/events;
// SSE 两种帧:update {version, tasks, stats} 与 openNewTask;
// X-Zdl-Version 响应头与 SSE version 同走单调门控,只渲染不倒退的快照(防进度条闪回)。
import { reactive } from "vue";
import { api, initSSEBase, sseURL } from "./lib/api.js";
import { magnetFallbackName, STATUS_TEXT } from "./lib/fmt.js";

export const store = reactive({
  tasks: [],
  stats: { totalSpeed: 0, totalUpSpeed: 0, rssUnread: 0 },
  cfg: null,
  currentView: "active",
  conn: true,        // SSE 连接状态(顶栏徽章)
  version: "",       // /api/ping 的后端版本(顶栏徽标)
  themeTick: 0,      // 主题切换计数:canvas 等读 CSS 变量的绘制层借此重绘
  viewTick: 0,       // 视图切换计数:任务列表借此重触发 view-swap 动画
  // 确认弹窗(全局单例,语义对齐旧 confirmBox)
  confirm: null,     // { title?, message, okLabel, secondaryLabel, onOk, onSecondary }
  toasts: [],        // { id, msg, cls }
  // 对话框动作注册表:组件挂载时注册自己的打开函数,供跨层调用
  // (顶栏按钮/任务卡动作/Go 侧 window.__* 入口)
  actions: {},
  detailId: null, // 详情抽屉当前任务(任务被删时由 acceptTick 自动关抽屉)
});

/* ---------------- Overlay 栈 ----------------
 * Escape 关「最上层」弹窗的语义按旧版 DOM 序复刻:旧版取 $$(".overlay.show")
 * 的最后一个,即 DOM 序最靠后者。RANK 即旧 index.html 的 overlay 先后顺序;
 * 叠窗(设置→封禁列表)在 RANK 中天然靠后,符合旧版关顶层的观察结果。 */
const OVERLAY_RANK = [
  "newTaskOverlay", "btOverlay", "chooseOverlay", "settingsOverlay",
  "rssFeedOverlay", "rssFolderOverlay", "rssPrevOverlay", "bansOverlay",
  "drawerOverlay", "trOverlay", "trAddOverlay", "clientBLOverlay",
  "peerAddOverlay", "trLogOverlay", "confirmOverlay",
];
export const ui = reactive({ flags: {}, stack: [] });

export function openOverlay(id) {
  if (ui.flags[id]) return;
  ui.flags[id] = true;
  ui.stack.push(id);
}

export function closeOverlay(id) {
  if (!ui.flags[id]) return;
  ui.flags[id] = false;
  const i = ui.stack.indexOf(id);
  if (i >= 0) ui.stack.splice(i, 1);
}

// Escape:关闭 DOM 序最靠前的显示中弹窗(=旧 $$(".overlay.show") 末位)
export function closeTopOverlay() {
  let top = null, rank = -1;
  for (const id of ui.stack) {
    const r = OVERLAY_RANK.indexOf(id);
    if (r > rank) { rank = r; top = id; }
  }
  if (top) closeOverlay(top);
}

export function confirmBox(message, { title, okLabel = "确认", onOk, secondaryLabel, onSecondary } = {}) {
  store.confirm = { title, message, okLabel, secondaryLabel, onOk, onSecondary };
}

/* ---------------- 快照门控 ---------------- */
let snapVer = 0;
export function acceptSnapshot(v) {
  if (!v || v < snapVer) return false;
  snapVer = v;
  return true;
}

/* ---------------- 总速度历史(顶栏速度图) ---------------- */
export const speedHistory = [];   // 总下载速度样本
export const upSpeedHistory = []; // 总上传速度样本(与上者同拍推入,长度恒一致)

export function pushSpeedHistory() {
  speedHistory.push(store.stats.totalSpeed || 0);
  upSpeedHistory.push(store.stats.totalUpSpeed || 0);
  if (speedHistory.length > 95) speedHistory.shift();
  if (upSpeedHistory.length > 95) upSpeedHistory.shift();
}

/* ---------------- Toast / 确认 ---------------- */
let toastSeq = 0;
export function toast(msg, cls = "") {
  const id = ++toastSeq;
  store.toasts.push({ id, msg, cls });
  setTimeout(() => {
    const t = store.toasts.find(x => x.id === id);
    if (t) t.fading = true; // Toasts.vue 据此过渡 opacity(对齐旧 4200ms 淡出)
  }, 4200);
  setTimeout(() => {
    const i = store.toasts.findIndex(x => x.id === id);
    if (i >= 0) store.toasts.splice(i, 1);
  }, 4600);
}

export const showErr = e => toast(e.message || String(e), "err");

/* ---------------- 最近使用的保存目录 ---------------- */
// 创建成功后回传本次手动指定的保存目录,后端去重、最新靠前、落盘;
// 响应带完整列表,直接刷新本地 cfg 供下拉展示。失败不影响创建流程。
export async function recordRecentDir(dir) {
  if (!dir) return;
  try {
    const r = await api("POST", "/api/recentdir", { dir });
    if (r.recentDirs && store.cfg) store.cfg.recentDirs = r.recentDirs;
  } catch (_) {}
}

export async function deleteRecentDir(dir) {
  try {
    const r = await api("POST", "/api/recentdir/delete", { dir });
    if (r.recentDirs && store.cfg) store.cfg.recentDirs = r.recentDirs;
  } catch (_) {}
}

/* ---------------- 文件夹选择 ---------------- */
export async function pickDir(title = "选择下载文件夹") {
  try {
    return await api("POST", "/api/pickdir", { title });
  } catch (e) {
    showErr(e);
    return { ok: false };
  }
}

/* ---------------- 视图过滤 ---------------- */
export function viewFilter(st) {
  switch (store.currentView) {
    case "all": return true;
    case "active": return ["running", "waiting", "paused", "error", "choosing"].includes(st);
    case "completed": return st === "completed";
    case "trash": return st === "canceled";
    case "seeding": return st === "seeding";
    case "rss": return false; // RSS 视图不展示任务卡片
  }
}

export function switchView(v) {
  store.currentView = v;
  store.viewTick++; // TaskList 据此重触发 view-swap 动画(移除→回流→加回)
}

/* ---------------- RSS 自动下载完成提醒 ---------------- */
const prevTaskStatus = new Map(); // id -> 上次快照状态;页面刚打开无前值,不补报历史
const rssActive = s => s === "waiting" || s === "running" || s === "paused" ||
  s === "choosing" || s === "error";

export function notifyRssAutoDone() {
  for (const t of store.tasks) {
    const prev = prevTaskStatus.get(t.id);
    prevTaskStatus.set(t.id, t.status);
    if (t.source === "rss-notify" && prev && rssActive(prev) &&
        (t.status === "completed" || t.status === "seeding")) {
      toast(`RSS 自动下载完成:${t.filename}`, "ok");
    }
  }
}

/* ---------------- 数据刷新 ---------------- */
export async function refreshTasks() {
  try {
    const [t, s] = await Promise.all([
      fetch("/api/tasks").then(r => {
        const v = parseInt(r.headers.get("X-Zdl-Version"), 10);
        return r.json().then(j => ({ j, v: isNaN(v) ? 0 : v }));
      }),
      api("GET", "/api/stats"),
    ]);
    if (!acceptSnapshot(t.v)) return;
    store.tasks = t.j;
    if (s) store.stats = s;
    acceptTick();
  } catch (_) { /* 离线时静默 */ }
}

// 快照被接受一次计一次:速度图与派生渲染统一以此为节拍(旧 render() 时机)
export function acceptTick() {
  notifyRssAutoDone();
  pushSpeedHistory();
  store.snapTick = (store.snapTick || 0) + 1;
  // 详情窗所指任务已被删除:关闭详情窗,避免残留一个轮询不存在的 id
  if (store.detailId && !store.tasks.some(t => t.id === store.detailId)) {
    store.actions.closeDrawer && store.actions.closeDrawer();
  }
  maybePromptChoose();
}

/* ---------------- SSE ---------------- */
export function setConn(ok) { store.conn = ok; }

export function connectSSE() {
  const es = new EventSource(sseURL());
  es.onopen = () => { setConn(true); fetchPendingNewTask(); };
  es.onmessage = e => {
    try {
      const msg = JSON.parse(e.data);
      if (msg.type === "update") {
        if (!acceptSnapshot(msg.version)) return;
        store.tasks = msg.tasks;
        store.stats = msg.stats;
        setConn(true);
        acceptTick();
      } else if (msg.type === "openNewTask") {
        // 核心通知「有待弹新建任务窗」:取走持久槽再弹(见 fetchPendingNewTask)
        fetchPendingNewTask();
      }
    } catch (_) { /* 忽略坏帧 */ }
  };
  es.onerror = () => setConn(false); // EventSource 自动重连
}

/* ---------------- 待弹创建窗槽 ---------------- */
let newTaskPollBusy = false;
// GET /api/ui/newtask 取走即清;对话框未注册前严禁消费,否则取走却不弹会
// 丢用户的关联启动/扩展请求
export async function fetchPendingNewTask() {
  if (newTaskPollBusy) return;
  if (!store.actions.openNewTask && !store.actions.openBTDialog) return;
  newTaskPollBusy = true;
  try {
    const p = await api("GET", "/api/ui/newtask");
    if (!p) return;
    if (p.seedConflict) { showSeedConflict(p); return; }
    if (p.magnet && store.actions.openBTDialog) {
      store.actions.openBTDialog(p.magnet);
    } else if (p.torrentPath && store.actions.openBTPrefill) {
      store.actions.openBTPrefill(p.torrentPath, p.dir || "", "");
    } else if (p.urls && p.urls.length && store.actions.openNewTask) {
      store.actions.openNewTask(p.urls, "", p.stay || false, { referer: p.referer, headers: p.headers });
    }
  } catch (_) { /* 核心离线等:忽略 */ }
  finally { newTaskPollBusy = false; }
}

// 种子自动打开冲突:弹确认框由用户决定;「创建任务」直接按种子的落盘目录建
// BT 任务(等价旧版:不再弹 BT 窗再问一遍保存目录;生成种子带 torrentGenerated
// 供引擎按 DeleteGeneratedTorrent 分流删除)
export function showSeedConflict(p) {
  confirmBox(
    "种子「" + p.seedName + "」已下载完成,但任务列表中已存在同名任务,未自动创建。是否仍要创建该任务?",
    {
      okLabel: "创建任务",
      onOk: async () => {
        try {
          const r = await api("POST", "/api/tasks/bt", {
            torrentPath: p.torrentPath,
            dir: p.dir || "",
            torrentGenerated: p.generated === true, // 生成种子按 DeleteGeneratedTorrent 分流删除
          });
          if (r.errors && r.errors.length) { toast(r.errors[0], "err"); return; }
          toast("已创建任务", "ok");
          refreshTasks();
        } catch (e) { showErr(e); }
      },
    },
  );
}

/* ---------------- 磁力就绪自动弹窗(choosing) ---------------- */
// 每任务每次页面会话只弹一次;对话框开着时不重复触发
const choosePrompted = new Set();
export function maybePromptChoose() {
  if (!store.tasks.some(t => t.kind === "bt" && t.status === "choosing")) return;
  if (ui.flags.btOverlay || ui.flags.chooseOverlay || ui.flags.drawerOverlay) return;
  for (const t of store.tasks) {
    if (t.kind === "bt" && t.status === "choosing" && !choosePrompted.has(t.id)) {
      choosePrompted.add(t.id);
      fetch("/api/ui/show").catch(() => {}); // 唤起原生窗口并置前取焦
      if (t.torrentFile && store.actions.openBTPrefill) store.actions.openBTPrefill(t.torrentFile, t.dir, t.id);
      else if (store.actions.openChooseDialog) store.actions.openChooseDialog(t.id);
      return;
    }
  }
}

/* ---------------- 任务操作 ---------------- */
export async function handleAction(act, id) {
  const t = store.tasks.find(x => x.id === id);
  try {
    switch (act) {
      case "pause": await api("POST", `/api/tasks/${id}/pause`); break;
      case "resume": await api("POST", `/api/tasks/${id}/resume`); break;
      case "reseed":
        try {
          await api("POST", `/api/tasks/${id}/reseed`);
          toast("已请求重新做种", "ok");
          refreshTasks();
        } catch (e) { showErr(e); }
        break;
      case "cancel":
        confirmBox("取消后已下载数据将被丢弃,确定取消该任务?", {
          okLabel: "取消任务",
          onOk: () => api("POST", `/api/tasks/${id}/cancel`).catch(showErr),
        });
        break;
      case "open": await api("POST", `/api/tasks/${id}/open`); break;
      case "stopseed":
        confirmBox("停止做种后任务将归入已完成,确定停止?", {
          okLabel: "停止做种",
          onOk: () => api("POST", `/api/tasks/${id}/stopseed`).catch(showErr),
        });
        break;
      case "detail": store.actions.openDrawer && store.actions.openDrawer(id); break;
      case "choosefiles": {
        // choosing:磁链产物种子预填创建对话框;无可见副本则回退改选窗口
        const t0 = store.tasks.find(x => x.id === id);
        if (t0 && t0.torrentFile && store.actions.openBTPrefill) store.actions.openBTPrefill(t0.torrentFile, t0.dir, id);
        else if (store.actions.openChooseDialog) store.actions.openChooseDialog(id);
        break;
      }
      case "retry": await api("POST", `/api/tasks/${id}/resume`); break;
      case "trash":
        confirmBox(`删除任务「${t?.filename || magnetFallbackName(t) || "未命名任务"}」的记录?`, {
          okLabel: "仅删除记录",
          onOk: () => api("DELETE", `/api/tasks/${id}`).catch(showErr),
          secondaryLabel: (t?.status === "completed" || t?.status === "seeding") ? "删除记录和文件" : undefined,
          onSecondary: () => api("DELETE", `/api/tasks/${id}?file=1`).catch(showErr),
        });
        break;
    }
  } catch (e) { showErr(e); }
}

// 点击文件名:用系统默认程序直接打开;可执行文件先经确认,防误触
const EXE_RE = /\.(exe|msi|bat|cmd|com|scr|pif|ps1|vbs)$/i;
export function openTaskFile(id) {
  const t = store.tasks.find(x => x.id === id);
  if (!t || (t.status !== "completed" && t.status !== "seeding")) return;
  const go = async () => {
    try {
      await api("POST", `/api/tasks/${id}/openfile`);
      toast("已调用系统程序打开", "ok");
    } catch (e) { showErr(e); }
  };
  if (EXE_RE.test(t.filename)) {
    confirmBox(`「${t.filename}」是可执行文件,直接运行可能有风险。确定要打开吗?`, {
      okLabel: "仍然打开",
      onOk: go,
    });
  } else {
    go();
  }
}

/* ---------------- 分类清扫 ---------------- */
export function clearView(view, label) {
  const status = view === "completed" ? "completed" : "canceled";
  const n = store.tasks.filter(t => t.status === status).length;
  if (!n) { toast("该分类没有可清扫的任务"); return; }
  confirmBox(`清扫「${label}」分类的全部 ${n} 条任务记录?`, {
    okLabel: "仅删除记录",
    onOk: () => doClear(view, false),
    secondaryLabel: view === "completed" ? "删除记录和文件" : undefined,
    onSecondary: () => doClear(view, true),
  });
}

async function doClear(view, file) {
  try {
    const r = await api("POST", "/api/tasks/clear", { view, file });
    toast(`已清扫 ${r.removed} 条任务记录` + (file ? "及已下载文件" : ""), "ok");
  } catch (e) { showErr(e); }
}

/* ---------------- 主题 ---------------- */
export function applyTheme(next) {
  // 切主题瞬间挂 theme-anim 类让颜色属性过渡,350ms 后摘除
  // (全局常开会让首屏渲染与无关状态变化都带上拖影)
  const root = document.documentElement;
  root.classList.add("theme-anim");
  root.dataset.theme = next;
  localStorage.setItem("ztheme", next);
  store.themeTick++;
  setTimeout(() => root.classList.remove("theme-anim"), 350);
}

/* ---------------- 启动 ---------------- */
export async function boot() {
  const saved = localStorage.getItem("ztheme");
  if (saved) document.documentElement.dataset.theme = saved;

  // Go 侧唤窗入口(WindowExecJS 注入,函数名是跨层契约,不可改名):
  // __zdWake=唤起后刷新;__wakeNewTask=取待弹创建窗;__zdOpenSettings=托盘直达设置
  window.__wakeNewTask = fetchPendingNewTask;
  window.__zdWake = () => { refreshTasks(); fetchPendingNewTask(); };
  window.__zdOpenSettings = () => store.actions.openSettings && store.actions.openSettings();

  try { store.cfg = await api("GET", "/api/config"); } catch (_) {}
  initSSEBase(store.cfg);
  api("GET", "/api/ping").then(p => {
    if (p && p.version) store.version = p.version;
  }).catch(() => {});

  connectSSE();
  setInterval(refreshTasks, 10000); // 轮询兜底
  setInterval(fetchPendingNewTask, 2000); // 槽为空时廉价空操作
  // 窗口从托盘/悬浮窗被唤起:WebView2 隐藏期间挂起 JS,快照停留旧值;
  // 变可见/获焦瞬间主动拉全量并取走待弹创建窗
  const wakeRefresh = () => { refreshTasks(); fetchPendingNewTask(); };
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") wakeRefresh();
  });
  window.addEventListener("focus", wakeRefresh);
  // SSE 之前先拉一次全量
  await refreshTasks();
}
