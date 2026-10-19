<script setup>
// 新建磁力/种子任务对话框:磁链 600ms 防抖解析(校验格式与哈希)、.torrent
// 客户端读字节转 base64、磁链 choosing 产物按本机路径预填、多文件种子文件树
// 选择(填充文件隐藏但始终选中)、同 InfoHash 重复任务确认。DOM 对齐旧 #btOverlay。
import { ref, computed, onMounted, onBeforeUnmount } from "vue";
import { api } from "../lib/api.js";
import { store, openOverlay, closeOverlay, toast, showErr, recordRecentDir, refreshTasks, pickDir, confirmBox } from "../store.js";
import { fmtBytes, STATUS_TEXT } from "../lib/fmt.js";
import { isPaddingFile } from "../lib/filetree.js";
import DirField from "./DirField.vue";
import FileTree from "./FileTree.vue";
import Overlay from "./Overlay.vue";
import ZSelect from "./ZSelect.vue";

const UNIT_OPTS = [{ value: "KB", label: "KB/s" }, { value: "MB", label: "MB/s" }];
const SEED_OPTS = [
  { value: "", label: "跟随全局" },
  { value: "ratio", label: "按分享率" },
  { value: "time", label: "按时长" },
  { value: "forever", label: "永久做种" },
];

const magnet = ref(""), torrentName = ref(""), dir = ref(""), dirUser = ref(false);
const upLimit = ref(0), upLimitUnit = ref("KB");
const seedPolicy = ref(""), seedRatio = ref(1), seedMinutes = ref(60);
const btChoose = ref(true);
const probe = ref({ show: false, cls: "probe-line", text: "" });
const submitDisabled = ref(true);
const fileSel = ref(null);          // FileTree 组件引用
const fileSelFiles = ref([]);       // 多文件种子的展示清单(已剔除填充文件)
const fileSelInitId = ref("");      // 文件树状态重置键
let parsedFull = [];                // 原始清单(含填充文件):部分选择时补其路径
const showFileSel = computed(() => fileSelFiles.value.length >= 2);
const browsing = ref(false);
const torrentInput = ref(null);

let torrentB64 = "";
let torrentPathRef = "";  // 预填的本机种子路径(磁链 choosing 产物),优先于 b64/磁力
let prefillTaskId = null; // 来源磁链任务:从预填创建成功后自动移除它
let parsedInfoHash = "";  // 当前解析出的 InfoHash(小写十六进制),提交前比对同名任务
let probeTimer = 0, probeSeq = 0;

const setProbe = (cls, text) => { probe.value = { show: true, cls: "probe-line " + cls, text }; };
const hideProbe = () => { probe.value = { show: false, cls: "probe-line", text: "" }; };

function renderFileSel(files) {
  // 填充文件不显示但保留在清单里:其字节参与相邻文件边界块的校验,
  // 选择性下载时也要一并选中(见 selectedFiles)
  parsedFull = files || [];
  fileSelInitId.value = "bt-dialog:" + Date.now(); // 变更键强制重置勾选/折叠
  fileSelFiles.value = parsedFull.filter(f => !isPaddingFile(f));
}
// 部分选择才传 files;null(全选)不传,与旧 selectedFiles 同语义
function selectedFiles() {
  if (!fileSelFiles.value.length || !fileSel.value) return null;
  const sel = fileSel.value.selectedPaths();
  if (sel === null) return null;
  const set = new Set(sel);
  // 被隐藏的填充文件不渲染但始终选中:边界块校验需要其字节
  for (const f of parsedFull) {
    if (isPaddingFile(f)) set.add(f.path.join("/"));
  }
  return [...set];
}

// 磁链解析:600ms 防抖(元数据未取前仅校验格式与哈希)
function onMagnetInput() {
  clearTimeout(probeTimer);
  const v = magnet.value.trim();
  const seq = ++probeSeq;
  torrentPathRef = ""; // 手输磁力:清除预填种子
  prefillTaskId = null;
  parsedInfoHash = "";
  if (!v) { hideProbe(); submitDisabled.value = !torrentB64; return; }
  probeTimer = setTimeout(async () => {
    try {
      const r = await api("POST", "/api/bt/parse", { magnet: v });
      if (seq !== probeSeq) return;
      torrentB64 = "";
      parsedInfoHash = r.infoHash;
      renderFileSel(null);
      torrentName.value = "";
      const parts = [`✓ InfoHash ${r.infoHash.slice(0, 16)}…`];
      if (r.name) parts.push(r.name);
      if (r.size > 0) parts.push(fmtBytes(r.size));
      parts.push(`${(r.trackers || []).length} 个 tracker`);
      setProbe("ok", parts.join(" · "));
      submitDisabled.value = false;
    } catch (e) {
      if (seq !== probeSeq) return;
      setProbe("bad", "✗ " + e.message);
      submitDisabled.value = true;
    }
  }, 600);
}

async function onTorrentFile(e) {
  const f = e.target.files[0];
  if (!f) return;
  torrentPathRef = ""; // 手选文件:清除预填种子
  prefillTaskId = null;
  parsedInfoHash = "";
  const buf = await f.arrayBuffer();
  let binary = "";
  const bytes = new Uint8Array(buf);
  for (let i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i]);
  const b64 = btoa(binary);
  try {
    const r = await api("POST", "/api/bt/parse", { torrentB64: b64 });
    torrentB64 = b64;
    parsedInfoHash = r.infoHash;
    renderFileSel(r.files || []);
    torrentName.value = f.name;
    magnet.value = "";
    hideProbe();
    setProbe("ok", `✓ ${r.name} · ${r.size > 0 ? fmtBytes(r.size) : "大小未知"} · ${r.files ? r.files.length + " 个文件" : "单文件"} · ${(r.trackers || []).length + (r.announce ? 1 : 0)} 个 tracker`);
    submitDisabled.value = false;
  } catch (err) {
    torrentB64 = "";
    renderFileSel(null);
    torrentName.value = "解析失败:" + f.name;
    setProbe("bad", "✗ " + err.message);
    submitDisabled.value = true;
  }
}

async function browse() {
  browsing.value = true;
  try {
    const r = await pickDir();
    if (r.ok && r.path) { dir.value = r.path; dirUser.value = true; }
  } finally { browsing.value = false; }
}

// 打开对话框;可选预填磁力(走既有解析防抖)——RSS 磁力附件/扩展右键/关联启动复用
function openBTDialog(magnetPre = "") {
  magnet.value = "";
  torrentName.value = "";
  if (torrentInput.value) torrentInput.value.value = "";
  dir.value = ""; dirUser.value = false;
  upLimit.value = 0; upLimitUnit.value = "KB";
  seedPolicy.value = "";
  submitDisabled.value = true;
  hideProbe();
  fileSelFiles.value = []; // 文件选择区随对话框重置
  torrentB64 = ""; torrentPathRef = ""; prefillTaskId = null; parsedInfoHash = "";
  try { btChoose.value = localStorage.getItem("btChoose") !== "0"; } catch (_) { btChoose.value = true; }
  openOverlay("btOverlay");
  if (magnetPre) {
    magnet.value = magnetPre;
    onMagnetInput(); // 走既有解析(600ms 防抖)
    // 光标就位并激活输入焦点(窗口程序化唤起时 webview 未必自动激活);
    // 下一帧再补一次,覆盖唤起的时序抖动
    requestAnimationFrame(() => magnetEl.value?.focus());
  }
}
const magnetEl = ref(null);

// 磁链 choosing 产物预填:解析种子路径,渲染文件树
async function openBTPrefilled(path, dirPrefill, magnetTaskId) {
  openBTDialog();
  torrentB64 = "";
  torrentPathRef = path;
  prefillTaskId = magnetTaskId || null;
  parsedInfoHash = "";
  magnet.value = "";
  torrentName.value = path;
  if (dirPrefill) dir.value = dirPrefill;
  submitDisabled.value = true;
  try {
    const r = await api("POST", "/api/bt/parse", { torrentPath: path });
    if (torrentPathRef !== path) return; // 期间用户已改道(手输/手选)
    parsedInfoHash = r.infoHash;
    renderFileSel(r.files || []);
    setProbe("ok", `✓ ${r.name} · ${r.size > 0 ? fmtBytes(r.size) : "大小未知"} · ${r.files ? r.files.length + " 个文件" : "单文件"} · ${(r.trackers || []).length + (r.announce ? 1 : 0)} 个 tracker`);
    submitDisabled.value = false;
  } catch (e) {
    if (torrentPathRef !== path) return;
    setProbe("bad", "✗ " + e.message);
  }
}

async function doCreate(body) {
  try {
    const r = await api("POST", "/api/tasks/bt", body);
    if (r.errors && r.errors.length) { toast(r.errors[0], "err"); return; }
    closeOverlay("btOverlay");
    const d = dir.value.trim();
    if (dirUser.value) recordRecentDir(d);
    if (prefillTaskId) {
      // 来源挂起任务(磁链/关联启动)使命完成:种子已固化到新任务与下载目录,移除记录
      api("DELETE", `/api/tasks/${prefillTaskId}`)
        .then(() => toast("已从种子创建下载任务,原挂起任务已移除", "ok"))
        .catch(() => toast("BT 任务已创建(原挂起任务移除失败,可手动删除)", "ok"));
    } else {
      toast("BT 任务已创建", "ok");
    }
    refreshTasks();
  } catch (e) { showErr(e); }
}

async function submit() {
  const d = dir.value.trim();
  const body = { dir: d };
  body.upLimit = Math.round((parseFloat(upLimit.value) || 0) * (upLimitUnit.value === "MB" ? 1048576 : 1024));
  if (seedPolicy.value === "ratio") body.seedRatio = parseFloat(seedRatio.value) || 0;
  if (seedPolicy.value === "time") body.seedMinutes = parseInt(seedMinutes.value, 10) || 0;
  if (seedPolicy.value === "forever") { body.seedRatio = 0; body.seedMinutes = 0; }
  if (torrentPathRef) {
    // 磁链 choosing 产物:按本机种子路径创建(避免 base64 大包)
    body.torrentPath = torrentPathRef;
    const sel = selectedFiles();
    if (sel) body.files = sel;
  } else if (torrentB64) {
    body.torrentB64 = torrentB64;
    const sel = selectedFiles();
    if (sel) body.files = sel; // 部分选择才传
  } else {
    body.magnet = magnet.value.trim();
    // 磁力:元数据就绪后是否挂起等待选择文件(仅多文件种子会挂起)
    body.choose = btChoose.value;
    try { localStorage.setItem("btChoose", body.choose ? "1" : "0"); } catch (_) {}
  }
  if (!body.torrentPath && !body.torrentB64 && !body.magnet) { toast("请输入磁力链接或选择种子文件", "err"); return; }
  // 同 InfoHash 已有任务:不静默建重复,交用户确认是否仍要创建(取消=留在此窗)
  if (parsedInfoHash) {
    const dup = store.tasks.find(t => t.kind === "bt" && t.infoHash && t.infoHash.toLowerCase() === parsedInfoHash.toLowerCase());
    if (dup) {
      confirmBox(`「${dup.filename || parsedInfoHash.slice(0, 16)}」已存在任务(${STATUS_TEXT[dup.status] || dup.status}),再建将产生重复。是否仍要创建?`, {
        title: "同名任务已存在",
        okLabel: "仍要创建",
        onOk: () => doCreate(body),
      });
      return;
    }
  }
  doCreate(body);
}

onMounted(() => {
  store.actions.openBTDialog = openBTDialog;
  store.actions.openBTPrefill = openBTPrefilled;
});
onBeforeUnmount(() => {
  if (store.actions.openBTDialog === openBTDialog) delete store.actions.openBTDialog;
  if (store.actions.openBTPrefill === openBTPrefilled) delete store.actions.openBTPrefill;
});
</script>

<template>
  <Overlay id="btOverlay">
    <div class="dialog">
      <h2><span class="accent-bar"></span>新建磁力 / 种子任务</h2>
      <div class="field">
        <label>磁力链接</label>
        <textarea id="btMagnet" ref="magnetEl" v-model="magnet" placeholder="magnet:?xt=urn:btih:…" spellcheck="false" @input="onMagnetInput"></textarea>
        <div v-show="probe.show" :class="probe.cls">{{ probe.text }}</div>
      </div>
      <div class="field">
        <label>或选择种子文件(.torrent)</label>
        <div class="input-row">
          <input type="text" id="btTorrentName" v-model="torrentName" placeholder="未选择文件" readonly>
          <button type="button" class="btn btn-ghost" id="btPickTorrent" @click="torrentInput.click()">选择文件…</button>
          <input type="file" id="btTorrentFile" ref="torrentInput" accept=".torrent,application/x-bittorrent" style="display:none" @change="onTorrentFile">
        </div>
      </div>
      <div class="field">
        <label>保存目录</label>
        <div class="input-row">
          <DirField v-model="dir" v-model:userDir="dirUser" />
          <button type="button" class="btn btn-ghost" id="btBrowse" :disabled="browsing" @click="browse">{{ browsing ? "选择中…" : "浏览…" }}</button>
        </div>
      </div>
      <div class="field-row">
        <div class="field">
          <label>上传限速(0 不限)</label>
          <div class="unit-row">
            <input type="number" v-model="upLimit" value="0" min="0" step="any">
            <ZSelect v-model="upLimitUnit" :options="UNIT_OPTS" />
          </div>
        </div>
        <div class="field">
          <label>做种策略(留空跟随全局设置)</label>
          <ZSelect v-model="seedPolicy" :options="SEED_OPTS" />
        </div>
      </div>
      <div class="field-row" v-show="seedPolicy === 'ratio' || seedPolicy === 'time'">
        <div class="field" v-show="seedPolicy === 'ratio'">
          <label>目标分享率</label>
          <input type="number" v-model="seedRatio" value="1" min="0" step="0.1">
        </div>
        <div class="field" v-show="seedPolicy === 'time'">
          <label>做种时长(分钟)</label>
          <input type="number" v-model="seedMinutes" value="60" min="1" step="1">
        </div>
      </div>
      <div class="hint-text" id="btMetaHint">提示:磁力任务需先获取元数据(经 tracker 或 DHT);无 tracker 且暂无 DHT 的磁链将无法解析。</div>
      <label class="hint-text" style="display:flex;align-items:center;gap:7px;cursor:pointer;font-size:12px">
        <input type="checkbox" id="btChoose" v-model="btChoose">
        磁力任务获取到元数据后弹出文件选择窗口(仅多文件种子)
      </label>
      <div id="btFileSel" v-show="showFileSel">
        <div class="sec-title" style="display:flex;align-items:center">
          选择下载文件(不选 = 单文件种子)
          <label style="margin-left:auto;font-size:12px;font-weight:400;display:flex;align-items:center;gap:5px;cursor:pointer">
            <input type="checkbox" id="btFileAll" :checked="fileSel && fileSel.selStat(fileSelFiles).all"
              @change="fileSel && fileSel.setAll($event.target.checked)"> 全选
          </label>
        </div>
        <div class="ft-scroll" style="max-height:240px">
          <FileTree ref="fileSel" :files="fileSelFiles" mode="sel" :init-id="fileSelInitId" />
        </div>
      </div>
      <div class="dialog-actions">
        <button class="btn btn-ghost" @click="closeOverlay('btOverlay')">取消</button>
        <button class="btn btn-primary" id="btSubmit" :disabled="submitDisabled" @click="submit">创建任务</button>
      </div>
    </div>
  </Overlay>
</template>
