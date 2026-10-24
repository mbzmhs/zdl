<script setup>
// 边下边播播放面板(全局单例 overlay):video 直连核心端口流端点。
// - 打开先用 Range: bytes=0-0 探测,失败(未选文件/会话未运行/v2 整校验)
//   直接展示引擎错误,不进播放器
// - 播放/缓冲:video 原生 controls;waiting/stalled 显示缓冲提示,
//   引擎按播放头自动重排 piece 优先级(拖动即新 Range,无需额外上报)
// - 1s 轮询 /stream/status:分块位图画布展示 已完成分块/引擎窗口带/头尾pin/
//   下载游标 + 播放头标记线——跳转后一眼对上"要播的位置"与"引擎在优先下哪里"
// - 关闭即 POST stop 还原纯下载优先级(空闲 5 分钟引擎也会自动回收)
import { ref, computed, reactive, watch, nextTick, onMounted, onBeforeUnmount } from "vue";
import { api, mediaURL } from "../lib/api.js";
import { store, ui, closeOverlay, toast } from "../store.js";
import { fmtBytes } from "../lib/fmt.js";
import { ms } from "../lib/icons.js";
import { isMediaFile, isPaddingFile } from "../lib/filetree.js";
import Overlay from "./Overlay.vue";

const p = computed(() => store.player);
// 流会话代次:每次切集/换任务单调 +1,拼进流 URL(&v=)。引擎拒收旧代次的
// 开流请求——快速切集后被放弃的播放器资源(旧 video 加载器超时重试、迟到
// 的连接)会把引擎会话拽回旧文件,位图/游标/pin 全部横跳(实测多集间反复
// 切换后缓冲时可见)。代次让这类请求在入口就被 409。
const playerEpoch = ref(0);
// file 为空 = 单文件种子任务卡快速入口:引擎解析唯一文件;首拍状态回填名称
const name = computed(() => {
  const f = p.value?.file || stat.value?.file || "";
  return f.includes("/") ? f.split("/").pop() : f;
});
const src = computed(() => p.value
  ? mediaURL(`/api/tasks/${p.value.taskId}/stream?file=${encodeURIComponent(p.value.file)}&v=${playerEpoch.value}`)
  : "");

const ready = ref(false);      // 探测通过,可进播放器
const errMsg = ref("");        // 探测失败原因(引擎文案)
const buffering = ref(false);  // video waiting/stalled
const stat = ref(null);        // /stream/status 快照
const videoEl = ref(null);     // 播放头位置(timeupdate→分块图标记线)
const bufCv = ref(null);       // 分块位图画布
const pendingSeek = ref(false); // seeking→seeked 过渡:红线贴 serveStart 而非均摊
let pollTimer = 0;
let cleaning = false; // 收尾单次保护(须先于下方 p-watch 声明:immediate 回调同步执行)

// 过期响应守卫:探测/状态轮询都是异步的,快速切集时旧请求可能后到——
// 不核对就把旧结果写进状态,面板会闪回旧一集(错误覆盖/位图错乱)
const amStale = key => !p.value || `${p.value.taskId}|${p.value.file}|${playerEpoch.value}` !== key;
const myKey = () => `${p.value.taskId}|${p.value.file}|${playerEpoch.value}`;

async function probe() {
  ready.value = false;
  errMsg.value = "";
  stat.value = null;
  const key = myKey();
  try {
    const resp = await fetch(src.value, { headers: { Range: "bytes=0-0" } });
    if (amStale(key)) return; // 期间已切集:结果作废(新会话的 probe 接管)
    if (!resp.ok) {
      const data = await resp.json().catch(() => ({}));
      errMsg.value = data.error || `HTTP ${resp.status}`;
      return;
    }
    resp.body?.cancel?.(); // 探测连接即刻关闭:播放器会开自己的连接
    ready.value = true;
  } catch (e) {
    if (!amStale(key)) errMsg.value = "无法连接流媒体端点: " + (e.message || e);
  }
}

async function tick() {
  if (!p.value) return;
  const key = myKey();
  let st = null;
  try { st = await api("GET", `/api/tasks/${p.value.taskId}/stream/status`); } catch {}
  if (amStale(key)) return; // 轮询期间切集:丢弃旧会话数据(位图横跳来源之一)
  stat.value = st;
  stepLowSpeed();
  // 缓冲提示自愈:视频可继续播(readyState≥HAVE_FUTURE_DATA)且不在 seek 时
  // 强制清除——stalled 等事件在缓冲充足时也会触发,播放从未中断就不会有
  // playing 事件来清提示(实测正常播放中提示挂死)
  const vd = videoEl.value;
  if (vd && buffering.value && vd.readyState >= 3 && !vd.seeking) buffering.value = false;
  drawBuf();
}

/* ---------------- 剧集播放列表 ----------------
 * 多文件种子的可播媒体文件列表(竖向,固定三行视口):点击切集(引擎重算
 * pin/窗口);本集播完自动连播下一集;接近尾声(>90%)把下一集头/尾元数据
 * piece 预热进最高优先级(切集秒开,不用等索引)。当前集始终滚到视口中间
 * (无前集时自然贴顶=第一行,末集自然贴底),自动切集后列表随之归位。 */
const playlist = ref([]);       // [{path, name, size}]
const warmed = reactive(new Set()); // 已预热的集(path;标记为小红点)
const plEl = ref(null);         // 列表容器(滚动定位)
let plItemEls = [];             // 行元素(函数 ref 按下标登记)
let lastTaskId = "";

const plItemRef = i => el => { if (el) plItemEls[i] = el; };
// 当前集滚到视口中间;offsetTop 为负时 clamp 到顶(第一集贴第一行),
// 超出滚动范围时 clamp 到底
function scrollPlCur(smooth = true) {
  const box = plEl.value, el = plItemEls[curIdx.value];
  if (!box || !el) return;
  const top = Math.max(0, el.offsetTop - (box.clientHeight - el.offsetHeight) / 2);
  box.scrollTo({ top, behavior: smooth ? "smooth" : "auto" });
}

async function loadPlaylist() {
  playlist.value = [];
  plItemEls = [];
  warmed.clear();
  const id = p.value?.taskId;
  if (!id) return;
  try {
    const d = await api("GET", `/api/tasks/${id}/detail`);
    playlist.value = (d.files || [])
      .filter(f => !isPaddingFile(f) && isMediaFile(f) && f.wanted !== false)
      .map(f => ({
        path: f.path.join("/"),
        name: f.path[f.path.length - 1] || "",
        size: f.length || 0,
      }));
    await nextTick();
    scrollPlCur(false); // 首次定位不加动画:打开即正确
  } catch {}
}
const curFile = computed(() => p.value?.file || stat.value?.file || "");
const curIdx = computed(() => playlist.value.findIndex(x => x.path === curFile.value));
const nextEp = computed(() => curIdx.value >= 0 ? playlist.value[curIdx.value + 1] : null);
// 切集/自动连播:curIdx 变化后行元素重挂,列表平滑归位到"当前集居中"
watch(curIdx, () => nextTick(() => scrollPlCur()));

function playEp(path) {
  if (!p.value || path === curFile.value) return;
  store.player = { taskId: p.value.taskId, file: path }; // 触发下方 watch:重新探测/换源
}
const onEnded = () => { if (nextEp.value) playEp(nextEp.value.path); };

async function warmNext() {
  const nx = nextEp.value;
  if (!nx || warmed.has(nx.path) || !p.value) return;
  warmed.add(nx.path);
  try { await api("POST", `/api/tasks/${p.value.taskId}/stream/warm`, { file: nx.path }); } catch {}
}

// 串流 URL 复制:交给 VLC/PotPlayer 等外部播放器(监听仅 127.0.0.1,本机可开)
async function copyStreamURL() {
  try {
    await navigator.clipboard.writeText(src.value);
    toast("串流 URL 已复制,可在 VLC/PotPlayer 中打开");
  } catch {
    toast("复制失败", "err");
  }
}

// 下载速度常显在"已缓冲"行尾;速度不足时变黄并附码率需求提示。带迟滞
//(进入 <0.9×码率,退出 >1.05×码率),EWMA 在阈值附近抖动不会造成
// 提示反复消失/重现。本集已缓冲 ≥99% 时不再提示(播完无缓冲压力)。
const lowSpeed = ref(false);
function stepLowSpeed() {
  const s = stat.value;
  if (!s || !(s.bitrate > 0) || (bufStat.value && bufStat.value.pct >= 99)) {
    lowSpeed.value = false;
    return;
  }
  if ((s.speed || 0) < s.bitrate * 0.9) lowSpeed.value = true;
  else if ((s.speed || 0) > s.bitrate * 1.05) lowSpeed.value = false;
}

watch(() => p.value && p.value.taskId + "|" + p.value.file, () => {
  clearInterval(pollTimer);
  if (p.value) {
    cleaning = false; // 新会话:重置收尾保护
    playerEpoch.value++; // 新代次:流 URL 变化,引擎拒收旧代次的迟到请求
    if (p.value.taskId !== lastTaskId) {
      lastTaskId = p.value.taskId;
      loadPlaylist();
    }
    probe();
    tick();
    pollTimer = setInterval(tick, 500);
  }
}, { immediate: true });
onBeforeUnmount(() => clearInterval(pollTimer));

/* ---------------- 分块位图画布 ----------------
 * 行高 26px:上层 0~22px 画已完成分块(密集时按列合并,全有=实蓝/部分=半蓝,
 * 同 BitfieldCanvas 口径);窗口带为底色淡蓝;头/尾 pin 区间底部橙色下划线;
 * 下载游标(引擎窗口起点)=白色细线;播放头(视频时间换算)=红色标记线。
 * 两条线重合即"引擎已切到所需位置",分开即窗口还在路上。 */
function drawBuf() {
  const cv = bufCv.value, s = stat.value;
  if (!cv) return;
  // 画布几何先行(任何状态都占位,高度恒定),随后清空:连接中 status 还是
  // 旧会话的数据(引擎在新 Range 到达时才切换),旧帧不清掉就会画出
  // 上一集的位图(实测切集缓冲时的"表格变成别的剧集")
  const dpr = window.devicePixelRatio || 1;
  const cssW = cv.clientWidth || cv.parentElement?.clientWidth || 300;
  if (cv.width !== Math.round(cssW * dpr)) {
    cv.width = Math.round(cssW * dpr);
    cv.height = Math.round(26 * dpr);
  }
  const ctx = cv.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  const w = cssW, h = 26;
  ctx.clearRect(0, 0, w, h);
  if (!ready.value || !s || !s.pieces || !(s.lastPiece >= s.firstPiece)) return;
  let raw;
  try { raw = atob(s.pieces); } catch { return; }
  const n = s.lastPiece - s.firstPiece + 1; // 文件区间分块数(位图下标 0 起)
  const xOf = i => i / n * w; // 分块下标(文件区间 0 起)→ 像素

  // 引擎优先窗口带 [cursor, cursor+ahead):淡蓝底色;前沿深度带
  // [cursor, cursor+depth):更亮一档——真正在被集中下载的活动区
  if (s.cursorPiece >= 0 && s.ahead > 0) {
    const c = Math.max(0, s.cursorPiece - s.firstPiece);
    const e = Math.min(n, c + s.ahead);
    ctx.fillStyle = "rgba(59,130,246,0.10)";
    ctx.fillRect(xOf(c), 0, xOf(e) - xOf(c), h - 3);
    if (s.depth > 0) {
      const de = Math.min(n, c + s.depth);
      ctx.fillStyle = "rgba(59,130,246,0.20)";
      ctx.fillRect(xOf(c), 0, xOf(de) - xOf(c), h - 3);
    }
  }
  // 已完成分块(列合并,密集时全有/部分区分)
  for (let x = 0; x < w; x++) {
    const st = Math.floor(x * n / w), en = Math.max(st + 1, Math.floor((x + 1) * n / w));
    let any = false, full = true;
    for (let i = st; i < en && i < n; i++) {
      if ((raw.charCodeAt(i >> 3) >> (7 - (i & 7))) & 1) any = true; else full = false;
    }
    if (any) {
      ctx.fillStyle = full ? "#3b82f6" : "rgba(59,130,246,0.45)";
      ctx.fillRect(x, 1, 1, h - 6);
    }
  }
  // 头/尾元数据 pin 区间:底部橙色下划线(尚未完成的分块更值得关注)
  const pin = (a, b) => {
    if (!(a >= 0) || !(b >= a)) return;
    const i0 = Math.max(0, a - s.firstPiece), i1 = Math.min(n - 1, b - s.firstPiece);
    if (i1 < i0) return;
    ctx.fillStyle = "#f59e0b";
    ctx.fillRect(xOf(i0), h - 4, Math.max(1, xOf(i1 + 1) - xOf(i0)), 2.5);
  };
  pin(s.pinHeadFirst, s.pinHeadLast);
  pin(s.pinTailFirst, s.pinTailLast);
  // 下载游标(引擎窗口起点):白色细线
  if (s.cursorPiece >= 0) {
    const c = s.cursorPiece - s.firstPiece;
    if (c >= 0 && c < n) {
      ctx.fillStyle = "rgba(255,255,255,0.9)";
      ctx.fillRect(Math.min(w - 1, xOf(c)), 0, 1.5, h - 3);
    }
  }
  // 播放头(视频时间→字节):优先用 buffered 时间区间与引擎服务字节区间对齐
  // 做内插(浏览器解码过 moov/cues 索引,局部码率近似恒定,VBR 下远准于全片
  // 均摊);拿不到服务区间时回退全片均摊(近似)。
  const vd = videoEl.value;
  if (vd && vd.duration > 0 && s.size > 0) {
    let fileOff = -1;
    const b = vd.buffered;
    if (s.serveEnd >= s.serveStart && (s.serveStart > 0 || s.serveEnd > 0)) {
      for (let i = 0; i < b.length; i++) {
        if (vd.currentTime >= b.start(i) - 0.05 && vd.currentTime <= b.end(i) + 0.05) {
          const span = b.end(i) - b.start(i);
          const t = span > 0 ? Math.min(1, Math.max(0, (vd.currentTime - b.start(i)) / span)) : 0;
          fileOff = s.serveStart + t * (s.serveEnd - s.serveStart);
          break;
        }
      }
    }
    // seek 过渡态(新位置尚无缓冲区间):serveStart 即 seek Range 的落点字节,
    // 红线贴它比全片均摊(拖动中错位的来源)准得多
    if (fileOff < 0 && pendingSeek.value && s.serveEnd >= s.serveStart && (s.serveStart > 0 || s.serveEnd > 0)) {
      fileOff = s.serveStart;
    }
    if (fileOff < 0) fileOff = vd.currentTime / vd.duration * s.size;
    const globalOff = (s.fileOffset || 0) + fileOff;
    const idx = Math.floor(globalOff / s.pieceLen) - s.firstPiece;
    if (idx >= 0 && idx < n) {
      ctx.fillStyle = "#ef4444";
      ctx.fillRect(Math.min(w - 2, xOf(idx)), 0, 2, h - 3);
    }
  }
}

// 播放头移动即时重画(timeupdate ~4Hz,画布极小无性能压力);接近尾声时
// 预热下一集(引擎把其头/尾元数据 piece 提到最高优先级,切集秒开);
// seeking 立刻刷新一次状态:seek Range 马上到达,serveStart 即落点字节,
// 红线贴住它,消除拖动期间回退均摊算法的错位过渡态
const onTimeUpdate = () => {
  drawBuf();
  const vd = videoEl.value;
  if (vd && vd.duration > 0 && vd.currentTime / vd.duration > 0.9) warmNext();
};
const onSeeking = () => { pendingSeek.value = true; tick(); };
const onSeeked = () => { pendingSeek.value = false; buffering.value = false; drawBuf(); };

const bufStat = computed(() => {
  // 连接中:status 还是上一集会话的数据,不展示(避免"已缓冲"短暂变成旧集)
  if (!ready.value) return null;
  const s = stat.value;
  if (!s || !s.pieces || !(s.lastPiece >= s.firstPiece)) return null;
  let bits;
  try { bits = atob(s.pieces); } catch { return null; }
  let done = 0;
  for (let i = 0; i < bits.length; i++) {
    const b = bits.charCodeAt(i);
    for (let k = 0; k < 8; k++) if (b & (0x80 >> k)) done++;
  }
  const total = s.lastPiece - s.firstPiece + 1;
  return { pct: Math.min(100, done / total * 100), done, total };
});

const onVWaiting = () => { buffering.value = true; };
const onVReady = () => { buffering.value = false; };
/* ---------------- 真全屏 ----------------
 * 桌面(Wails)内用窗口级全屏(WindowFullscreen/Unfullscreen,Windows/macOS
 * 均原生支持,退出自动还原窗体);浏览器内回退 video 元素全屏。
 * 双击拦截默认行为(WebView2 的元素全屏只能填满应用窗口)。 */
const fsByApp = ref(false);
const wailsRuntime = () => (typeof window !== "undefined" && window.runtime && window.runtime.WindowFullscreen)
  ? window.runtime : null;

async function toggleFullscreen() {
  const rt = wailsRuntime();
  if (rt) {
    if (!fsByApp.value) {
      await rt.WindowFullscreen();
      fsByApp.value = true;
    } else {
      await rt.WindowUnfullscreen();
      fsByApp.value = false;
    }
    return;
  }
  const vd = videoEl.value;
  if (!vd) return;
  if (!document.fullscreenElement) await vd.requestFullscreen().catch(() => {});
  else await document.exitFullscreen().catch(() => {});
}
const onDblClick = e => { e.preventDefault(); toggleFullscreen(); };
const fsActive = computed(() => fsByApp.value || elFs.value);

// WebView2 的原生双击全屏无法被 preventDefault 抑制(实测双击会与我们自己的
// 切换同时触发元素全屏,窗口全屏随即被误关):桌面(Wails)环境把元素全屏
// 一律"吸收"掉——仅退出、绝不切换,窗口级全屏的进/出统一由双击和自家
// 按钮负责。原生全屏按钮已用 CSS 隐藏。浏览器环境保持原生元素全屏。
const elFs = ref(false); // 元素全屏态(浏览器环境图标切换用)
function onDocFsChange() {
  elFs.value = !!document.fullscreenElement;
  if (!wailsRuntime() || !document.fullscreenElement) return;
  document.exitFullscreen().catch(() => {});
}
onMounted(() => document.addEventListener("fullscreenchange", onDocFsChange));

// Esc 语义分层:全屏态第一次 Esc 只退全屏,再按才关面板。App 的全局 Esc
// 关闭最上层 overlay 是 bubble 阶段的 document 监听,这里以 capture 阶段
// 监听拦截并 stopPropagation,抢在它之前(浏览器元素全屏下 Esc 通常被浏览器
// 自行消费退出、走不到这里;Wails 窗口全屏必经此处)。
function onKeydown(e) {
  if (e.key !== "Escape") return;
  if (elFs.value) { e.stopPropagation(); e.preventDefault(); document.exitFullscreen().catch(() => {}); return; }
  if (fsByApp.value) { e.stopPropagation(); e.preventDefault(); toggleFullscreen(); }
}
onMounted(() => document.addEventListener("keydown", onKeydown, true));

// 统一收尾:停流播、还原窗口全屏。close() 与"Esc/遮罩关闭 overlay"两条
// 路径都走到这里(后者绕过组件方法,靠 overlay 标志位 watch 兜住)
async function cleanup() {
  if (cleaning) return;
  cleaning = true;
  const id = p.value?.taskId;
  store.player = null;
  if (fsByApp.value) {
    fsByApp.value = false;
    const rt = wailsRuntime();
    if (rt) await rt.WindowUnfullscreen().catch(() => {});
  }
  if (id) { try { await api("POST", `/api/tasks/${id}/stream/stop`); } catch {} }
}
watch(() => ui.flags.playerOverlay, shown => {
  if (!shown && p.value) cleanup();
});
async function close() {
  closeOverlay("playerOverlay");
  await cleanup();
}
onBeforeUnmount(() => {
  clearInterval(pollTimer);
  document.removeEventListener("fullscreenchange", onDocFsChange);
  document.removeEventListener("keydown", onKeydown, true);
  if (p.value) cleanup();
});
</script>

<template>
  <Overlay id="playerOverlay">
    <div class="dialog player-dialog" :class="{ 'player-fs': fsByApp, 'has-pl': playlist.length > 1 }">
      <h2><span class="accent-bar"></span>边下边播<span class="dim player-name" :title="p?.file">{{ name }}</span>
        <span class="player-h2-actions">
          <button class="icon-btn" title="复制串流 URL(可在 VLC/PotPlayer 打开)" @click="copyStreamURL"
            v-html="ms('link')"></button>
          <button class="icon-btn" :title="fsActive ? '退出全屏' : '全屏'" @click="toggleFullscreen"
            v-html="fsActive ? ms('fullscreen_exit') : ms('fullscreen')"></button>
          <button class="icon-btn" style="margin-left:0" title="关闭" @click="close">✕</button>
        </span>
      </h2>
      <div v-if="errMsg" class="player-error">
        <p>{{ errMsg }}</p>
        <p class="dim">可在任务详情的文件页勾选该文件后重试;暂停中的任务恢复运行后再播。</p>
      </div>
      <div v-else-if="ready" class="player-body">
        <video ref="videoEl" :src="src" controls autoplay playsinline
          @waiting="onVWaiting" @timeupdate="onTimeUpdate"
          @seeking="onSeeking" @seeked="onSeeked" @dblclick="onDblClick" @ended="onEnded"
          @playing="onVReady" @canplay="onVReady" @pause="onVReady"></video>
        <div v-if="buffering" class="player-hint">缓冲中…正在优先下载播放位置附近的分块</div>
      </div>
      <div v-else class="player-error"><p class="dim">正在连接流媒体端点…</p></div>
      <!-- 剧集列表常驻挂载(不随 ready 切换卸载):切集时 probe 会让视频区回到
        "连接中",列表若跟着卸载重挂会丢滚动位置(curIdx watcher 空跑后无人
        再触发);连接中/出错态也保持可见——出错时可直接点其他集重试 -->
      <div v-if="playlist.length > 1" ref="plEl" class="player-pl">
        <button v-for="(ep, i) in playlist" :key="ep.path" :ref="plItemRef(i)" class="pl-item"
          :class="{ cur: i === curIdx, warmed: warmed.has(ep.path), ready: warmed.has(ep.path) && stat?.warmReady }"
          :title="ep.name + (ep.size ? ' · ' + fmtBytes(ep.size) : '') + (warmed.has(ep.path) ? (stat?.warmReady ? ' · 下一集元数据已就绪' : ' · 预热中(元数据最高优先下载)') : '')"
          @click="playEp(ep.path)">
          <span class="pl-idx">{{ i + 1 }}</span><span class="pl-name">{{ ep.name }}</span>
        </button>
      </div>
      <!-- 统计区同样常驻(连接中显示"读取缓冲状态…"占位):任何状态下
        分块条/图例/速度行都占位,面板高度恒定,切集不闪 -->
      <div class="player-stat">
        <canvas ref="bufCv" class="piece-canvas"></canvas>
        <div class="player-legend dim">
          <span><i class="lg lg-done"></i>已完成</span>
          <span><i class="lg lg-win"></i>优先窗口</span>
          <span><i class="lg lg-front"></i>活动前沿</span>
          <span><i class="lg lg-pin"></i>头尾元数据</span>
          <span><i class="lg lg-cur"></i>下载游标</span>
          <span><i class="lg lg-play"></i>播放头</span>
        </div>
        <span class="dim">{{ bufStat ? `已缓冲 ${bufStat.pct.toFixed(0)}%(分块 ${bufStat.done}/${bufStat.total})` : "读取缓冲状态…"
          }}<template v-if="stat"> · {{ fmtBytes(stat.size || 0) }} · <span class="pl-speed" :class="{ low: lowSpeed }">↓ {{ fmtBytes(stat.speed || 0) }}/s<template v-if="lowSpeed">(低于码率 {{ fmtBytes(stat.bitrate) }}/s,可能反复缓冲)</template></span></template></span>
      </div>
    </div>
  </Overlay>
</template>
