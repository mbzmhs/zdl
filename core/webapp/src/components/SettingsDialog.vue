<script setup>
// 设置对话框:左侧分类导航(滑动指示条)+ 右侧六个分节 + 系统日志页 3s 自动刷新。
// 保存:PUT /api/config 部分合并;开机自启与文件关联各自落库,失败不阻断其余。
// DOM 对齐旧 #settingsOverlay。
import { ref, reactive, computed, watch, nextTick, onMounted, onBeforeUnmount } from "vue";
import { api } from "../lib/api.js";
import { store, ui, openOverlay, closeOverlay, toast, showErr, confirmBox, pickDir } from "../store.js";
import Overlay from "./Overlay.vue";
import ZSelect from "./ZSelect.vue";
import ZTimePicker from "./ZTimePicker.vue";

const NUM_OPTS = (n, step = 1) => Array.from({ length: n }, (_, k) => ({ value: (k + 1) * step, label: String((k + 1) * step) }));
const UNIT_OPTS = [{ value: "KB", label: "KB/s" }, { value: "MB", label: "MB/s" }];
const PROXY_MODE_OPTS = [
  { value: "none", label: "不使用代理" },
  { value: "manual", label: "手动配置" },
  { value: "env", label: "跟随系统环境变量" },
];
const RSS_AUTODL_OPTS = [
  { value: "off", label: "不自动下载" },
  { value: "notify", label: "自动下载(完成后提醒)" },
  { value: "silent", label: "静默下载" },
];
import BansDialog from "./BansDialog.vue";
import ClientBLDialog from "./ClientBLDialog.vue";

const navEl = ref(null), bodyEl = ref(null);
const cat = ref("general");
const browsing = ref(false);

/* 表单状态(打开时从服务端快照整体填充) */
const f = reactive({
  downloadDir: "", maxActive: 1, maxConn: 8,
  globalLimit: 0, globalLimitUnit: "KB", userAgent: "",
  skipTLS: false, openBrowser: false, autoStart: false, showFloat: true,
  proxyMode: "none", proxyUrl: "", followView: "",
  proxyHTTP: true, proxyTracker: true, proxyPeer: true, proxyWebSeed: true, dohDNS: true,
  autoOpenTorrent: false,
  assocMagnet: false, assocTorrent: false,
  btPort: 0, btMaxPeers: 60, btUpSpeed: 0, btUpSpeedUnit: "KB",
  btSeedRatio: 1, btSeedMinutes: 0,
  btDHT: true, btPEX: true, btAutoSeed: true,
  btDelGenerated: false, btDelSource: false,
  btAutoBanPoison: true, btPoisonRate: 80, btPoisonStrikes: 3,
  btTrackers: "", btAutoTrackers: true,
  btSpoofUA: true, btSpoofName: "BitComet 1.99",
  dhtNodes: "",
  rssAuto: true, rssInterval: 60, rssMaxItems: 200, rssOnStart: true,
  rssWindow: false, rssWindowStart: "00:00", rssWindowEnd: "23:59", rssAutoDl: "off",
});
const bansCount = ref(0);
// 文件关联:平台(决定交互形态)与 macOS 接管实况(LaunchServices 真实查询)
const assocPlatform = ref("");
const assocMagnetOk = ref(false), assocTorrentOk = ref(false);
const clientBLDraft = ref([]);
const sysLog = ref("加载中…");
let sysLogSig = "";
const logLoading = ref(false);

const syncSpoof = computed(() => f.btSpoofUA);
const spoofNameView = computed({
  get() {
    // 勾选继承 UA 时只读并实时跟随 User-Agent 输入框
    if (f.btSpoofUA) return f.userAgent.trim() || "BitComet 1.99";
    return f.btSpoofName;
  },
  set(v) { f.btSpoofName = v; },
});

// 滑动指示条(几何逻辑对齐旧 makeSlideInd;宿主=按钮本身)
let ind = null;
function syncInd() {
  const act = navEl.value?.querySelector(".settings-nav-item.active");
  if (!ind) return;
  if (!act) { ind.style.opacity = "0"; return; }
  ind.style.opacity = "1";
  ind.style.width = act.offsetWidth + "px";
  ind.style.height = act.offsetHeight + "px";
  ind.style.transform = `translate(${act.offsetLeft}px, ${act.offsetTop}px)`;
}
function pickCat(c) {
  cat.value = c;
  bodyEl.value.scrollTop = 0;
  if (c === "log") refreshSysLog(); // 切到日志页即拉一次最新
  nextTick(syncInd);
}

// 系统日志(设置 → 系统日志):签名比对——无新日志不重写,保住选中文本
function refreshSysLog() {
  if (!sysLogSig && !logLoading.value) { sysLog.value = "加载中…"; logLoading.value = true; }
  api("GET", "/api/log?n=200").then(r => {
    logLoading.value = false;
    const sig = (r.lines || []).join("\n");
    if (sig === sysLogSig) return; // 内容未变不动 DOM,保住选中文本
    sysLogSig = sig;
    sysLog.value = sig || "(暂无日志)";
  }).catch(e => {
    logLoading.value = false;
    sysLog.value = "加载失败：" + ((e && e.message) || String(e));
  });
}
// 系统日志自动刷新:仅当设置窗开着且当前是日志分类时拉取
let logTimer = 0;
watch(() => ui.flags.settingsOverlay, v => {
  if (v) logTimer = setInterval(() => {
    if (ui.flags.settingsOverlay && cat.value === "log") refreshSysLog();
  }, 3000);
  else { clearInterval(logTimer); logTimer = 0; }
});

// 跟随系统代理的只读回显:实时检测环境变量 / Windows / macOS 系统代理
function refreshFollowProxy() {
  api("GET", "/api/sysproxy").then(s => {
    let text = "未检测到代理(将直连)";
    if (s.source === "env") text = "环境变量 " + s.envVar + " → " + (s.url || s.addr);
    else if (s.source === "windows") text = "Windows 系统代理 → " + (s.url || s.addr);
    else if (s.source === "macos") text = "macOS 系统代理 → " + (s.url || s.addr);
    f.followView = text;
  }).catch(() => { f.followView = ""; });
}

const showFollow = computed(() => f.proxyMode === "env");
const showUrlWrap = computed(() => f.proxyMode === "manual");
const showUses = computed(() => f.proxyMode !== "none");

async function browseDir() {
  browsing.value = true;
  try {
    const r = await pickDir();
    if (r.ok && r.path) f.downloadDir = r.path;
  } finally { browsing.value = false; }
}

async function openSettingsDialog() {
  refreshSysLog(); // 打开设置即刷新系统日志(若正停在日志页立即可见)
  let cfg;
  try { cfg = await api("GET", "/api/config"); } catch (e) { showErr(e); return; }
  store.cfg = cfg;
  f.downloadDir = cfg.downloadDir;
  f.maxActive = cfg.maxActiveTasks;
  f.maxConn = cfg.maxConnections;
  // 全局限速换算回显:优先整 MB,其次整 KB
  const gl = cfg.globalLimit || 0;
  if (gl > 0 && gl % 1048576 === 0) { f.globalLimit = gl / 1048576; f.globalLimitUnit = "MB"; }
  else if (gl > 0 && gl % 1024 === 0) { f.globalLimit = gl / 1024; f.globalLimitUnit = "KB"; }
  else if (gl > 0) { f.globalLimit = +(gl / 1024).toFixed(2); f.globalLimitUnit = "KB"; }
  else { f.globalLimit = 0; f.globalLimitUnit = "KB"; }
  f.userAgent = cfg.userAgent;
  f.proxyMode = cfg.proxy?.mode || "none";
  f.proxyUrl = cfg.proxy?.url || "";
  if (f.proxyMode === "env") refreshFollowProxy();
  // 文件关联开关以注册表实况为准;"自动打开种子"跟随配置
  f.autoOpenTorrent = !!cfg.autoOpenTorrent;
  api("GET", "/api/assoc").then(a => {
    assocPlatform.value = a.platform || "";
    assocMagnetOk.value = !!a.magnet; assocTorrentOk.value = !!a.torrent;
    f.assocMagnet = !!a.magnet; f.assocTorrent = !!a.torrent;
  }).catch(() => {});
  // 粒度开关:未配置(旧版本)视为开启
  f.proxyHTTP = cfg.proxy?.forHTTP !== false;
  f.proxyTracker = cfg.proxy?.forTracker !== false;
  f.proxyPeer = cfg.proxy?.forPeer !== false;
  f.proxyWebSeed = cfg.proxy?.forWebSeed !== false;
  f.dohDNS = cfg.proxy?.dohDNS !== false;
  f.skipTLS = !!cfg.skipTLSVerify;
  f.openBrowser = !!cfg.openBrowser;
  f.showFloat = cfg.showFloatWindow !== false;
  // BT 设置
  const bt = cfg.bt || {};
  f.btPort = bt.port ?? 0;
  f.btMaxPeers = bt.maxPeers || 60;
  const up = bt.upSpeed || 0;
  if (up > 0 && up % 1048576 === 0) { f.btUpSpeed = up / 1048576; f.btUpSpeedUnit = "MB"; }
  else if (up > 0 && up % 1024 === 0) { f.btUpSpeed = up / 1024; f.btUpSpeedUnit = "KB"; }
  else { f.btUpSpeed = up ? +(up / 1024).toFixed(2) : 0; f.btUpSpeedUnit = "KB"; }
  f.btSeedRatio = bt.seedRatio ?? 1;
  f.btSeedMinutes = bt.seedMinutes ?? 0;
  f.btDHT = bt.dht !== false;
  f.btPEX = bt.pex !== false;
  f.btAutoSeed = bt.autoSeed !== false;
  f.btDelGenerated = bt.deleteGeneratedTorrent === true;
  f.btDelSource = bt.deleteSourceTorrent === true;
  f.btAutoBanPoison = bt.autoBanPoison !== false;
  f.btPoisonRate = bt.poisonRate || 80;
  f.btPoisonStrikes = bt.poisonStrikes || 3;
  clientBLDraft.value = (bt.clientBlacklist || []).slice();
  f.btTrackers = (bt.publicTrackers || []).join("\n");
  f.btAutoTrackers = bt.autoAddTrackers !== false;
  f.btSpoofUA = bt.spoofUA !== false;
  f.btSpoofName = bt.spoofName || "BitComet 1.99";
  // RSS 订阅设置
  const rss = cfg.rss || {};
  f.rssAuto = rss.autoUpdate !== false;
  f.rssInterval = rss.intervalMin || 60;
  f.rssMaxItems = rss.maxItems || 200;
  f.rssOnStart = rss.updateOnStart !== false;
  f.rssWindow = !!rss.windowEnabled;
  f.rssWindowStart = rss.windowStart || "00:00";
  f.rssWindowEnd = rss.windowEnd || "23:59";
  f.rssAutoDl = ["off", "notify", "silent"].includes(rss.autoDl) ? rss.autoDl : "off";
  // 开机自启独立于 config.json,状态以注册表为准;读取失败不阻塞弹窗
  api("GET", "/api/autostart").then(a => { f.autoStart = !!a.enabled; }).catch(() => {});
  // DHT 路由表健康度:节点数实时展示(未入网通常需 1-2 分钟)
  api("GET", "/api/stats").then(st => {
    const n = +st.dhtNodes || 0;
    f.dhtNodes = n > 0 ? "DHT 路由表 " + n + " 个节点" : "DHT 未入网(启动后约 1-2 分钟;或 DHT 未启用)";
  }).catch(() => {});
  // 封禁计数实时点亮按钮:有记录时按钮缀上条数
  api("GET", "/api/bt/bans").then(r => {
    bansCount.value = (r.bans || []).length;
  }).catch(() => {});
  openOverlay("settingsOverlay");
  // 弹窗此前 display:none,指示条几何可能过期,显示后重定位一帧
  requestAnimationFrame(syncInd);
}

function openBans() {
  openOverlay("bansOverlay");
  bansEl.value?.render();
}
const bansEl = ref(null);
function blSaved(patterns) {
  clientBLDraft.value = patterns;
}

async function save() {
  const gv = parseFloat(f.globalLimit) || 0;
  const body = {
    downloadDir: f.downloadDir.trim(),
    maxActiveTasks: parseInt(f.maxActive, 10),
    maxConnections: parseInt(f.maxConn, 10),
    globalLimit: Math.round(gv * (f.globalLimitUnit === "MB" ? 1048576 : 1024)),
    userAgent: f.userAgent.trim(),
    proxy: {
      mode: f.proxyMode,
      url: f.proxyUrl.trim(),
      forHTTP: f.proxyHTTP,
      forTracker: f.proxyTracker,
      forPeer: f.proxyPeer,
      forWebSeed: f.proxyWebSeed,
      dohDNS: f.dohDNS,
    },
    skipTLSVerify: f.skipTLS,
    openBrowser: f.openBrowser,
    showFloatWindow: f.showFloat,
    autoOpenTorrent: f.autoOpenTorrent,
    bt: {
      port: parseInt(f.btPort, 10) || 0,
      maxPeers: parseInt(f.btMaxPeers, 10) || 60,
      upSpeed: Math.round((parseFloat(f.btUpSpeed) || 0) * (f.btUpSpeedUnit === "MB" ? 1048576 : 1024)),
      seedRatio: parseFloat(f.btSeedRatio) || 0,
      seedMinutes: parseInt(f.btSeedMinutes, 10) || 0,
      dht: f.btDHT,
      pex: f.btPEX,
      autoSeed: f.btAutoSeed,
      deleteGeneratedTorrent: f.btDelGenerated,
      deleteSourceTorrent: f.btDelSource,
      autoBanPoison: f.btAutoBanPoison,
      poisonRate: Math.min(100, Math.max(50, parseInt(f.btPoisonRate, 10) || 80)),
      poisonStrikes: Math.min(10, Math.max(1, parseInt(f.btPoisonStrikes, 10) || 3)),
      publicTrackers: f.btTrackers.split("\n").map(x => x.trim()).filter(Boolean),
      autoAddTrackers: f.btAutoTrackers,
      spoofUA: f.btSpoofUA,
      spoofName: f.btSpoofUA ? "" : f.btSpoofName.trim(),
    },
    rss: {
      autoUpdate: f.rssAuto,
      intervalMin: parseInt(f.rssInterval, 10) || 60,
      updateOnStart: f.rssOnStart,
      windowEnabled: f.rssWindow,
      windowStart: f.rssWindowStart || "00:00",
      windowEnd: f.rssWindowEnd || "23:59",
      maxItems: parseInt(f.rssMaxItems, 10) || 200,
      autoDl: f.rssAutoDl,
    },
  };
  try {
    store.cfg = await api("PUT", "/api/config", body);
  } catch (e) { showErr(e); return; }
  // 开机自启保存失败不影响其余设置(已成功落盘)
  try { await api("POST", "/api/autostart", { enabled: f.autoStart }); }
  catch (e) { showErr(e); }
  // 文件关联:按开关写注册表;失败不阻断其余设置,响应为注册表实况。
  // macOS 走「接管系统关联」按钮的即时流,不走保存(避免旧开关值重复弹系统框)。
  if (assocPlatform.value !== "darwin") {
    try {
      const a = await api("POST", "/api/assoc", { magnet: f.assocMagnet, torrent: f.assocTorrent });
      f.assocMagnet = !!a.magnet;
      f.assocTorrent = !!a.torrent;
    } catch (e) { showErr(e); }
  }
  closeOverlay("settingsOverlay");
  toast("设置已保存", "ok");
}

// macOS:请求 LaunchServices 接管默认应用——系统弹确认框,用户点「使用
// ZDownloader」即生效;延迟回读真实状态,未生效则提示去点弹窗。
async function takeOverAssoc() {
  try {
    await api("POST", "/api/assoc", { magnet: true, torrent: true });
    setTimeout(async () => {
      try {
        const s = await api("GET", "/api/assoc");
        assocMagnetOk.value = !!s.magnet; assocTorrentOk.value = !!s.torrent;
        if (s.magnet && s.torrent) toast("已接管系统关联", "ok");
        else toast("请在系统弹窗中点「使用 ZDownloader」确认", "");
      } catch (_) { /* 忽略 */ }
    }, 1500);
  } catch (e) { showErr(e); }
}

async function refreshAllFeeds() {
  try {
    await api("POST", "/api/rss/refresh");
    toast("已开始更新全部订阅", "ok");
  } catch (e) { showErr(e); }
}

onMounted(() => { store.actions.openSettings = openSettingsDialog; });
onBeforeUnmount(() => {
  if (store.actions.openSettings === openSettingsDialog) delete store.actions.openSettings;
  if (logTimer) clearInterval(logTimer);
});
</script>

<script>
export default { name: "SettingsDialog" };
</script>

<template>
  <Overlay id="settingsOverlay">
    <div class="dialog settings-dialog">
      <div class="settings-head">
        <h2><span class="accent-bar"></span>设置</h2>
      </div>
      <div class="settings-layout">
        <nav class="settings-nav" ref="navEl">
          <span class="nav-slide-ind"></span>
          <button class="settings-nav-item" :class="{ active: cat === 'general' }" @click="pickCat('general')">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>
            通用
          </button>
          <button class="settings-nav-item" :class="{ active: cat === 'proxy' }" @click="pickCat('proxy')">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M3 12h18"/><path d="M12 3a14.5 14.5 0 0 1 0 18 14.5 14.5 0 0 1 0-18"/></svg>
            代理
          </button>
          <button class="settings-nav-item" :class="{ active: cat === 'bt' }" @click="pickCat('bt')">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2a10 10 0 1 0 10 10"/><path d="M12 7a5 5 0 1 0 5 5"/><circle cx="12" cy="12" r="1" fill="currentColor"/></svg>
            BT 下载
          </button>
          <button class="settings-nav-item" :class="{ active: cat === 'rss' }" @click="pickCat('rss')">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M4 11a9 9 0 0 1 9 9"/><path d="M4 4a16 16 0 0 1 16 16"/><circle cx="5" cy="19" r="1.6" fill="currentColor" stroke="none"/></svg>
            RSS 订阅
          </button>
          <button class="settings-nav-item" :class="{ active: cat === 'assoc' }" @click="pickCat('assoc')">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/></svg>
            文件关联
          </button>
          <button class="settings-nav-item" :class="{ active: cat === 'log' }" @click="pickCat('log')">
            <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M6 3h9l4 4v14H6z"/><path d="M14 3v5h5"/><path d="M9 12h7M9 16h7"/></svg>
            系统日志
          </button>
        </nav>
        <div class="settings-body" ref="bodyEl">
          <!-- 通用 -->
          <div class="settings-pane" v-show="cat === 'general'">
            <div class="field">
              <label>默认下载目录</label>
              <div class="input-row">
                <input type="text" v-model="f.downloadDir">
                <button type="button" class="btn btn-ghost" :disabled="browsing" @click="browseDir">{{ browsing ? "选择中…" : "浏览…" }}</button>
              </div>
            </div>
            <div class="field-row">
              <div class="field">
                <label>同时下载任务数</label>
                <ZSelect v-model="f.maxActive" :options="NUM_OPTS(10)" />
              </div>
              <div class="field">
                <label>默认线程数</label>
                <ZSelect v-model="f.maxConn" :options="NUM_OPTS(64)" />
              </div>
            </div>
            <div class="field-row">
              <div class="field">
                <label>全局限速(0 为不限速)</label>
                <div class="unit-row">
                  <input type="number" v-model="f.globalLimit" min="0" step="any">
                  <ZSelect v-model="f.globalLimitUnit" :options="UNIT_OPTS" />
                </div>
              </div>
              <div class="field">
                <label>User-Agent</label>
                <input type="text" v-model="f.userAgent">
              </div>
            </div>
            <div class="pane-sep"></div>
            <div class="switch-row">
              <label class="switch"><input type="checkbox" v-model="f.skipTLS"><span class="track"></span></label>
              <span class="lbl">跳过 TLS 证书验证(自签名场景)</span>
            </div>
            <div class="switch-row">
              <label class="switch"><input type="checkbox" v-model="f.openBrowser"><span class="track"></span></label>
              <span class="lbl">启动时自动打开管理界面</span>
            </div>
            <div class="switch-row">
              <label class="switch"><input type="checkbox" v-model="f.autoStart"><span class="track"></span></label>
              <span class="lbl">开机自动启动(登录后静默常驻托盘)</span>
            </div>
            <div class="switch-row">
              <label class="switch"><input type="checkbox" v-model="f.showFloat"><span class="track"></span></label>
              <span class="lbl">悬浮下载进度窗<span class="q"><i>?</i><span class="q-tip">桌面版:置顶小窗常显总速度与任务进度。</span></span></span>
            </div>
          </div>

          <!-- 代理 -->
          <div class="settings-pane" v-show="cat === 'proxy'">
            <div class="field">
              <label>代理模式<span class="q" v-show="f.proxyMode === 'env'"><i>?</i><span class="q-tip">优先读取环境变量 HTTPS_PROXY / HTTP_PROXY / ALL_PROXY,同时识别 Windows 系统代理(Internet 选项);NO_PROXY / 例外列表生效,下方粒度开关同样约束各用途;DHT 始终直连,udp tracker 仅 SOCKS5 代理支持中继(HTTP 代理无 UDP 通道)。</span></span></label>
              <ZSelect v-model="f.proxyMode" :options="PROXY_MODE_OPTS" @change="f.proxyMode === 'env' && refreshFollowProxy()" />
              <div class="hint-text" v-show="showFollow">检测到的代理(只读):</div>
              <input type="text" v-model="f.followView" readonly v-show="showFollow" style="margin-top:2px" placeholder="未检测到代理(将直连)">
            </div>
            <div class="field" v-show="showUrlWrap">
              <label>代理地址<span class="q"><i>?</i><span class="q-tip">支持 HTTP(S) 与 SOCKS5 代理;内网地址始终直连。</span></span></label>
              <input type="text" v-model="f.proxyUrl" placeholder="http://127.0.0.1:7890 或 socks5://user:pass@127.0.0.1:1080">
            </div>
            <div class="pane-sep"></div>
            <div v-show="showUses">
              <div class="switch-row">
                <label class="switch"><input type="checkbox" v-model="f.proxyHTTP"><span class="track"></span></label>
                <span class="lbl">使用代理进行 HTTP 下载</span>
              </div>
              <div class="switch-row">
                <label class="switch"><input type="checkbox" v-model="f.proxyTracker"><span class="track"></span></label>
                <span class="lbl">使用代理连接 Tracker<span class="q"><i>?</i><span class="q-tip">udp tracker 直连失败时经 SOCKS5 中继重试(HTTP 代理无 UDP 通道);DHT 始终直连。</span></span></span>
              </div>
              <div class="switch-row">
                <label class="switch"><input type="checkbox" v-model="f.proxyPeer"><span class="track"></span></label>
                <span class="lbl">使用代理连接 Peer</span>
              </div>
              <div class="switch-row">
                <label class="switch"><input type="checkbox" v-model="f.proxyWebSeed"><span class="track"></span></label>
                <span class="lbl">使用代理连接长效种子(WebSeed)</span>
              </div>
              <div class="switch-row">
                <label class="switch"><input type="checkbox" v-model="f.dohDNS"><span class="track"></span></label>
                <span class="lbl">DNS 解析走代理(DoH)<span class="q"><i>?</i><span class="q-tip">经 DoH 加密解析域名,防本地 DNS 污染;tracker 解析与 DHT 引导均使用。</span></span></span>
              </div>
            </div>
          </div>

          <!-- BT 下载 -->
          <div class="settings-pane" v-show="cat === 'bt'">
            <div class="field-row">
              <div class="field">
                <label>BT 监听端口<span class="q"><i>?</i><span class="q-tip">0 = 自动选择;端口被占用时自动顺延。所有 BT 任务共享同一监听端口,入站连接按种子自动路由;NAT 穿透映射此端口。</span></span></label>
                <input type="number" v-model="f.btPort" min="0" max="65535" placeholder="6881">
              </div>
              <div class="field">
                <label>单任务最大 peer 连接数</label>
                <ZSelect v-model="f.btMaxPeers" :options="NUM_OPTS(20, 10)" />
              </div>
            </div>
            <div class="field">
              <label>全局上传限速(0 为不限速)</label>
              <div class="unit-row">
                <input type="number" v-model="f.btUpSpeed" min="0" step="any">
                <ZSelect v-model="f.btUpSpeedUnit" :options="UNIT_OPTS" />
              </div>
            </div>
            <div class="field-row">
              <div class="field">
                <label>做种目标分享率(0 = 不按比率停)</label>
                <input type="number" v-model="f.btSeedRatio" min="0" step="0.1">
              </div>
              <div class="field">
                <label>最短做种时长(分钟,0 = 不按时长停)</label>
                <input type="number" v-model="f.btSeedMinutes" min="0" step="1">
              </div>
            </div>
            <div class="pane-sep"></div>
            <div class="switch-row">
              <label class="switch"><input type="checkbox" v-model="f.btDHT"><span class="track"></span></label>
              <span class="lbl">使用 DHT<span class="q"><i>?</i><span class="q-tip">无 tracker 磁力链接的 peer 来源:经公共路由节点(DHT 网络)发现 peer。</span></span><span class="hint-text" style="margin:0 0 0 8px">{{ f.dhtNodes || "DHT 路由表: –" }}</span></span>
            </div>
            <div class="switch-row">
              <label class="switch"><input type="checkbox" v-model="f.btPEX"><span class="track"></span></label>
              <span class="lbl">使用 PEX<span class="q"><i>?</i><span class="q-tip">peer 交换:与已连接的 peer 互换更多 peer 地址;私有种子自动禁用。</span></span></span>
            </div>
            <div class="switch-row">
              <label class="switch"><input type="checkbox" v-model="f.btAutoSeed"><span class="track"></span></label>
              <span class="lbl">任务完成后自动做种<span class="q"><i>?</i><span class="q-tip">完成后继续上传,分享率或最短时长任一达标即停止;两项均为 0 则一直做种。</span></span></span>
            </div>
            <div class="switch-row">
              <label class="switch"><input type="checkbox" v-model="f.btDelGenerated"><span class="track"></span></label>
              <span class="lbl">创建 BT 任务后删除生成的种子文件<span class="q"><i>?</i><span class="q-tip">本程序生成的种子(磁链抓取产物、HTTP 下载完成的种子自动转建)在任务创建后删除其 .torrent 副本。引擎保留内部元数据存档,下载/续传/做种不受影响,详情页可随时「另存为种子」取回。</span></span></span>
            </div>
            <div class="switch-row">
              <label class="switch"><input type="checkbox" v-model="f.btDelSource"><span class="track"></span></label>
              <span class="lbl">删除双击/手动打开的种子源文件<span class="q"><i>?</i><span class="q-tip">双击 .torrent 或手动选路径创建任务后,删除你自有的那个源种子文件。默认关闭——多数用户想保留自己的种子库,需要清理时再开。</span></span></span>
            </div>
            <div class="switch-row">
              <label class="switch"><input type="checkbox" v-model="f.btAutoBanPoison"><span class="track"></span></label>
              <span class="lbl">自动封禁PCDN客户端<span class="q"><i>?</i><span class="q-tip">两类特征自动拉黑:① piece 校验失败按块来源追责,同一 IP 累计供坏块达下方「坏块宽容次数」(默认 3 次)封禁(整块唯一来源同样按次数累计);对端要维持在「白名单」里才豁免,白名单按下方 piece 有效率阈值动态判定——校验通过才算好块,混着掺坏块把有效率拖到阈值以下就当场踢出、按一般 peer 记打击。② 只取不予:下载期间长期索取我们上 MB 数据、回传不足其 1/16 且不到 512KB(对端自报的下载进度可以伪造,不作依据;做种期改按「回报零且进度静止」判定),观察满 10 分钟即封。封禁名单全局共享、重启仍生效;点「封禁列表」可查看与解除。</span></span></span>
              <button type="button" class="btn btn-ghost btn-sm" id="btnBTBans" style="margin-left:4px" @click="openBans">封禁列表{{ bansCount > 0 ? ` (${bansCount})` : "" }}</button>
            </div>
            <div class="field">
              <label>白名单 piece 有效率阈值(%)<span class="q"><i>?</i><span class="q-tip">该 IP 参与过的 piece 中通过校验的比例达到此值才豁免自动封禁(参与片数就是「连接」表里的 piece有效率列);低于即踢出白名单,之后累计坏块达宽容次数即封。默认 80,可设 50 - 100。阈值越高越严格:100 表示掺一片坏块就失去豁免。无样本(从未参与过 piece 校验)的 peer 不在白名单内。</span></span></label>
              <input type="number" v-model="f.btPoisonRate" min="50" max="100" step="1" value="80">
            </div>
            <div class="field">
              <label>坏块宽容次数(次)<span class="q"><i>?</i><span class="q-tip">同一 IP 累计供坏块达到该次数才自动封禁(白名单 peer 有效率达标期间不计打击,跌破阈值后从零开始累计)。默认 3,可设 1 - 10:1 = 掺一片坏块就封,最严但对我方 uTP 接收瑕疵误伤的容忍度也最低;调大更宽容。只改封禁节奏,不改判定口径;只想观察不封禁请用上方总开关。</span></span></label>
              <input type="number" v-model="f.btPoisonStrikes" min="1" max="10" step="1" value="3">
            </div>
            <div class="field">
              <label>客户端黑名单<span class="q"><i>?</i><span class="q-tip">按对端客户端名称自动封禁,快速拉黑迅雷等吸血端与 PCDN 常用客户端。支持通配符:星号匹配任意长度、问号匹配单个字符,大小写不敏感(如 *Xunlei*、*Thunder*)。命中即写入全局封禁名单并断开连接,保存立即生效——新连接在握手期直接拒绝,已连上的热封断开。注意:有 PCDN 伪装成 qBittorrent 4.6.7,拉黑 qB 系列会误伤大量正常 peer,请精确到版本。</span></span></label>
              <button type="button" class="btn btn-ghost btn-sm" id="btnBTClientBL" style="align-self:flex-start" @click="openOverlay('clientBLOverlay')">管理{{ clientBLDraft.length ? `(${clientBLDraft.length} 条)` : "" }}…</button>
            </div>
            <div class="field">
              <label>公共 tracker 列表<span class="q"><i>?</i><span class="q-tip">每行一个。自动添加到所有 BT 任务;磁力链接抓取元数据时始终使用。</span></span></label>
              <textarea v-model="f.btTrackers" class="mono" rows="6" spellcheck="false" placeholder="udp://tracker.opentrackr.org:1337/announce"></textarea>
            </div>
            <div class="switch-row">
              <label class="switch"><input type="checkbox" v-model="f.btAutoTrackers"><span class="track"></span></label>
              <span class="lbl">自动补充公共 tracker<span class="q"><i>?</i><span class="q-tip">把上方公共 tracker 列表自动添加到 BT 任务(下载阶段生效;私有种子始终不添加)。</span></span></span>
            </div>
            <div class="pane-sep"></div>
            <div class="switch-row">
              <label class="switch"><input type="checkbox" v-model="f.btSpoofUA"><span class="track"></span></label>
              <span class="lbl">伪装名继承 User-Agent<span class="q"><i>?</i><span class="q-tip">勾选后 BT 客户端名称跟随上方 User-Agent 设置;取消勾选可单独填写。</span></span></span>
            </div>
            <div class="field">
              <div class="q-row">
                <input type="text" v-model="spoofNameView" :readonly="f.btSpoofUA" placeholder="BitComet 1.99" spellcheck="false">
                <span class="q q-up q-right"><i>?</i><span class="q-tip">BT 对外呈现的客户端名称(扩展握手 / tracker 通告 UA / 制种信息),peer_id 前缀固定为 BitComet 格式以通过兼容性白名单。</span></span>
              </div>
            </div>
          </div>

          <!-- RSS -->
          <div class="settings-pane" v-show="cat === 'rss'">
            <div class="switch-row">
              <label class="switch"><input type="checkbox" v-model="f.rssAuto"><span class="track"></span></label>
              <span class="lbl">定时自动更新订阅<span class="q"><i>?</i><span class="q-tip">按下方间隔抓取全部订阅的新条目;关闭后仅在 RSS 页面手动点「全部更新」。</span></span></span>
            </div>
            <div class="field-row">
              <div class="field">
                <label>更新间隔(分钟)<span class="q"><i>?</i><span class="q-tip">5 - 1440 分钟;每轮只抓取已到期的订阅,带 ETag/Last-Modified 条件请求,内容没变不重复下载。</span></span></label>
                <input type="number" v-model="f.rssInterval" min="5" max="1440" step="5">
              </div>
              <div class="field">
                <label>每个订阅保留条目数<span class="q"><i>?</i><span class="q-tip">20 - 5000;超出后从最旧条目开始丢弃(已读状态随之清理)。</span></span></label>
                <input type="number" v-model="f.rssMaxItems" min="20" max="5000" step="10">
              </div>
            </div>
            <div class="switch-row">
              <label class="switch"><input type="checkbox" v-model="f.rssOnStart"><span class="track"></span></label>
              <span class="lbl">启动后立即更新一次<span class="q"><i>?</i><span class="q-tip">关闭时启动后先静默一个完整间隔再开始自动更新,避免开机瞬间集中抓取。</span></span></span>
            </div>
            <div class="switch-row">
              <label class="switch"><input type="checkbox" v-model="f.rssWindow"><span class="track"></span></label>
              <span class="lbl">仅在指定时段内自动更新<span class="q"><i>?</i><span class="q-tip">时段外不自动抓取(手动刷新不受限制);支持跨天时段,如 23:00 - 06:00。</span></span></span>
            </div>
            <div class="field-row" v-show="f.rssWindow">
              <div class="field">
                <label>开始时间</label>
                <ZTimePicker v-model="f.rssWindowStart" />
              </div>
              <div class="field">
                <label>结束时间</label>
                <ZTimePicker v-model="f.rssWindowEnd" />
              </div>
            </div>
            <div class="field">
              <label>自动下载<span class="q"><i>?</i><span class="q-tip">每次更新订阅时自动下载新条目的资源链接(附件/磁力),条目页面不会被下载;触发后条目置为已读,完成后可提醒。全局默认值,各订阅可在添加/编辑订阅时单独设为「继承全局」或指定其他模式。</span></span></label>
              <ZSelect v-model="f.rssAutoDl" :options="RSS_AUTODL_OPTS" />
            </div>
            <div class="pane-sep"></div>
            <div class="switch-row">
              <span class="lbl">立即更新全部订阅</span>
              <button type="button" class="btn btn-ghost btn-sm" style="margin-left:auto" @click="refreshAllFeeds">立即更新</button>
            </div>
            <div class="hint-text">订阅抓取遵循「代理」页的设置(HTTP 用途开关);磁力与种子附件可直接创建 BT 任务。</div>
          </div>

          <!-- 文件关联 -->
          <div class="settings-pane" v-show="cat === 'assoc'">
            <!-- macOS:LaunchServices 真接管,点按钮弹系统确认框;开关形式不适用 -->
            <template v-if="assocPlatform === 'darwin'">
              <div class="switch-row">
                <span class="lbl" style="display:flex;align-items:center;gap:8px">magnet: 链接
                  <span style="font-size:12px;font-weight:600" :style="{ color: assocMagnetOk ? 'var(--ok)' : 'var(--text-faint)' }">{{ assocMagnetOk ? "已接管 ✓" : "未接管" }}</span>
                </span>
              </div>
              <div class="switch-row">
                <span class="lbl" style="display:flex;align-items:center;gap:8px">种子文件(.torrent)
                  <span style="font-size:12px;font-weight:600" :style="{ color: assocTorrentOk ? 'var(--ok)' : 'var(--text-faint)' }">{{ assocTorrentOk ? "已接管 ✓" : "未接管" }}</span>
                </span>
              </div>
              <div class="switch-row" style="gap:8px;flex-wrap:wrap">
                <button type="button" class="btn btn-ghost" style="padding:5px 14px;font-size:13px" @click="takeOverAssoc">接管系统关联…</button>
              </div>
              <div class="hint-text">点「接管系统关联」后 macOS 会弹出确认框,点「使用 ZDownloader」即完成,无需进设置页手动切换;取消关联 macOS 不允许应用代劳,需在系统设置 → 桌面与程序坞 中更改。</div>
            </template>
            <!-- Windows:注册表同步写入,双开关粒度保留 -->
            <template v-else>
              <div class="switch-row">
                <label class="switch"><input type="checkbox" v-model="f.assocMagnet"><span class="track"></span></label>
                <span class="lbl">磁力链接关联<span class="q"><i>?</i><span class="q-tip">注册 magnet: 协议:点击磁力链接时由本程序接手——程序在运行则转交并在界面弹出创建窗口(可选保存目录与下载文件),未运行则自动启动后同样弹出。写入当前用户注册表(HKCU),无需管理员权限;若此前已为磁力链接指定其他程序,需在系统「默认应用」设置中切换一次。</span></span></span>
              </div>
              <div class="switch-row">
                <label class="switch"><input type="checkbox" v-model="f.assocTorrent"><span class="track"></span></label>
                <span class="lbl">种子文件关联(.torrent)<span class="q"><i>?</i><span class="q-tip">注册 .torrent 打开方式:双击种子文件用本程序打开,界面弹出创建窗口(可选保存目录与下载文件)后开始下载。写入当前用户注册表(HKCU),无需管理员权限;若系统此前已为 .torrent 指定其他默认程序,需在资源管理器「打开方式」中确认一次。</span></span></span>
              </div>
            </template>
            <div class="switch-row">
              <label class="switch"><input type="checkbox" v-model="f.autoOpenTorrent"><span class="track"></span></label>
              <span class="lbl">自动打开下载完成的种子文件<span class="q"><i>?</i><span class="q-tip">本程序 HTTP 下载的 .torrent 完成后,自动按其内容创建 BT 任务;同 InfoHash 已有任务时不重复创建。仅对本程序下载的种子生效,随时可在此关闭。</span></span></span>
            </div>
            <div class="hint-text" v-if="assocPlatform !== 'darwin'">关联写入当前用户注册表,保存设置时生效;命令已指向其他程序的关联不会被改动。</div>
          </div>

          <!-- 系统日志 -->
          <div class="settings-pane" v-show="cat === 'log'">
            <section>
              <div class="sec-title">系统日志</div>
              <div class="hint-text">全局尾部日志(最近 200 行);设置窗口打开期间每 3 秒自动刷新。</div>
              <div style="display:flex;justify-content:flex-end;margin-top:8px">
                <button class="btn btn-ghost" style="padding:4px 12px;font-size:13px" @click="refreshSysLog">刷新</button>
              </div>
              <pre class="log-view" style="max-height:52vh">{{ sysLog }}</pre>
            </section>
          </div>
        </div>
      </div>
      <div class="dialog-actions settings-actions">
        <button class="btn btn-ghost" @click="closeOverlay('settingsOverlay')">取消</button>
        <button class="btn btn-primary" id="stSave" @click="save">保存</button>
      </div>
    </div>
    <BansDialog ref="bansEl" @count="bansCount = $event" />
    <ClientBLDialog :draft="clientBLDraft" @saved="blSaved" />
  </Overlay>
</template>
