<script setup>
// 新建下载任务对话框(HTTP/HLS,支持批量):链接预解析(防抖 200ms)、
// 高级选项折叠、限速单位换算、目录下拉。DOM 对齐旧 #newTaskOverlay。
import { ref, onMounted, onBeforeUnmount } from "vue";
import { api } from "../lib/api.js";
import { store, openOverlay, closeOverlay, toast, showErr, recordRecentDir, switchView, pickDir } from "../store.js";
import { fmtBytes } from "../lib/fmt.js";
import DirField from "./DirField.vue";
import Overlay from "./Overlay.vue";
import ZSelect from "./ZSelect.vue";

const CONN_OPTS = Array.from({ length: 64 }, (_, k) => ({ value: k + 1, label: String(k + 1) }));
const UNIT_OPTS = [{ value: "KB", label: "KB/s" }, { value: "MB", label: "MB/s" }];
const PROXY_OPTS = [
  { value: "", label: "跟随全局设置" },
  { value: "none", label: "直连(不走代理)" },
  { value: "custom", label: "自定义…" },
];

const connections = ref(8);
const dir = ref(""), dirUser = ref(false);
const urls = ref(""), filename = ref("");
const referer = ref(""), headersText = ref("");
const speedLimit = ref("0"), speedLimitUnit = ref("KB");
const checksum = ref(""), proxy = ref(""), proxyCustom = ref("");
const advOpen = ref(false);
const probe = ref({ show: false, cls: "probe-line", text: "" });
let probeTimer = 0, probeSeq = 0;
let ntStay = false;

function parseHeaders() {
  const headers = {};
  for (const line of headersText.value.split("\n")) {
    const i = line.indexOf(":");
    if (i > 0) headers[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  return headers;
}
function proxyValue() {
  return proxy.value === "custom" ? proxyCustom.value.trim() : proxy.value;
}

function resetProbe() {
  clearTimeout(probeTimer);
  probeSeq++;
  probe.value = { show: false, cls: "probe-line", text: "" };
}
async function runProbe() {
  const lines = urls.value.split("\n").map(s => s.trim()).filter(Boolean);
  if (!lines.length) { resetProbe(); return; }
  const seq = ++probeSeq;
  probe.value = { show: true, cls: "probe-line", text: "解析中…" };
  try {
    // 高级选项里已填的请求参数一并带上,保证解析条件与实际下载一致
    const r = await api("POST", "/api/probe", {
      url: lines[0], referer: referer.value.trim(), headers: parseHeaders(), proxy: proxyValue(),
    });
    if (seq !== probeSeq) return;
    if (!r.ok) {
      probe.value = { show: true, cls: "probe-line bad", text: "✗ " + (r.error || "无法解析该链接") };
      return;
    }
    const bits = [r.filename || "未知文件名"];
    if (r.hls) {
      bits.push("HLS 流媒体(分段下载)");
    } else {
      bits.push(r.size > 0 ? fmtBytes(r.size) : "大小未知");
      bits.push(r.acceptRanges ? "支持断点续传" : "不支持断点续传");
    }
    if (lines.length > 1) bits.push(`共 ${lines.length} 个链接,取首个解析`);
    probe.value = { show: true, cls: "probe-line ok", text: "✓ " + bits.join(" · ") };
    // 不把识别出的文件名自动填进「文件名」框:该框对整批链接生效,预填第一
    // 条的名字会让批量里其它文件被套上同一个名字(同名覆盖的诱因)
  } catch (e) {
    if (seq !== probeSeq) return;
    probe.value = { show: true, cls: "probe-line bad", text: "✗ " + (e.message || "解析失败") };
  }
}
function onUrlsInput() {
  clearTimeout(probeTimer);
  probeTimer = setTimeout(runProbe, 200);
}

// 打开新建任务窗;urls 预填(每行一个:批量下载 / RSS 资源链接复用此入口)。
// stay=true:来自 RSS 的下载,创建成功后停留在当前视图,不跳「下载中」。
// params:{referer, headers} 可选:浏览器扩展唤起时预填高级项。
function openDialog(urlsPrefill = [], filenamePrefill = "", stay = false, params = null) {
  ntStay = stay;
  urls.value = urlsPrefill.join("\n");
  filename.value = filenamePrefill;
  connections.value = store.cfg?.maxConnections || 8;
  dir.value = ""; dirUser.value = false;
  referer.value = ""; headersText.value = "";
  speedLimit.value = "0"; speedLimitUnit.value = "KB";
  checksum.value = ""; proxy.value = ""; proxyCustom.value = "";
  if (params) {
    if (params.referer) referer.value = params.referer;
    if (params.headers && Object.keys(params.headers).length)
      headersText.value = Object.entries(params.headers).map(([k, v]) => k + ": " + v).join("\n");
  }
  resetProbe();
  openOverlay("newTaskOverlay");
  // 预填链接立即解析,不等输入防抖(用上预填的 Referer/Cookie)
  if (urlsPrefill.length) runProbe();
  requestAnimationFrame(() => urlsEl.value?.focus());
}

const urlsEl = ref(null);
const browsing = ref(false);
async function browse() {
  browsing.value = true;
  try {
    const r = await pickDir();
    if (r.ok && r.path) { dir.value = r.path; dirUser.value = true; }
  } finally { browsing.value = false; }
}

async function submit() {
  const list = urls.value.split("\n").map(s => s.trim()).filter(Boolean);
  if (!list.length) { toast("请输入至少一个下载链接", "err"); return; }
  const d = dir.value.trim();
  const body = {
    urls: list,
    filename: filename.value.trim(),
    dir: d,
    connections: parseInt(connections.value, 10) || 0,
    referer: referer.value.trim(),
    headers: parseHeaders(),
    // 与全局限速一致的单位换算:界面 KB/s 或 MB/s,后端字节/秒
    speedLimit: Math.round((parseFloat(speedLimit.value) || 0) *
      (speedLimitUnit.value === "MB" ? 1048576 : 1024)),
    checksum: checksum.value.trim(),
    proxy: proxyValue(),
  };
  try {
    const resp = await api("POST", "/api/tasks", body);
    closeOverlay("newTaskOverlay");
    toast(`已创建 ${resp.tasks.length} 个任务`, "ok");
    (resp.errors || []).forEach(e => toast(e, "err"));
    if (resp.tasks.length && dirUser.value) recordRecentDir(d);
    if (resp.tasks.length && !ntStay) switchView("active");
  } catch (e) { showErr(e); }
}

onMounted(() => {
  connections.value = store.cfg?.maxConnections || 8;
  store.actions.openNewTask = openDialog;
});
onBeforeUnmount(() => { if (store.actions.openNewTask === openDialog) delete store.actions.openNewTask; });
</script>

<template>
  <Overlay id="newTaskOverlay">
    <div class="dialog">
      <h2><span class="accent-bar"></span>新建下载任务</h2>
      <div class="field">
        <label>下载链接(每行一个,支持批量)</label>
        <textarea id="ntUrls" ref="urlsEl" v-model="urls" placeholder="https://example.com/file.zip" spellcheck="false" @input="onUrlsInput"></textarea>
        <div v-show="probe.show" :class="probe.cls">{{ probe.text }}</div>
      </div>
      <div class="field-row">
        <div class="field">
          <label>保存文件名(留空自动识别)</label>
          <input type="text" v-model="filename" placeholder="自动识别">
        </div>
        <div class="field">
          <label>线程数</label>
          <ZSelect v-model="connections" :options="CONN_OPTS" />
        </div>
      </div>
      <div class="field">
        <label>保存目录</label>
        <div class="input-row">
          <DirField v-model="dir" v-model:userDir="dirUser" />
          <button type="button" class="btn btn-ghost" id="ntBrowse" :disabled="browsing" @click="browse">{{ browsing ? "选择中…" : "浏览…" }}</button>
        </div>
      </div>
      <button class="adv-toggle" :class="{ open: advOpen }" @click="advOpen = !advOpen">
        高级选项
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"><path d="M6 9l6 6 6-6"/></svg>
      </button>
      <div class="adv-panel" :class="{ show: advOpen }">
        <div class="field">
          <label>Referer</label>
          <input type="text" v-model="referer" placeholder="留空则不发送">
        </div>
        <div class="field">
          <label>自定义请求头(每行一个,格式 Key: Value)</label>
          <textarea v-model="headersText" placeholder="Cookie: session=..." spellcheck="false"></textarea>
        </div>
        <div class="field-row">
          <div class="field">
            <label>限速(0 不限)</label>
            <div class="unit-row">
              <input type="number" v-model="speedLimit" value="0" min="0" step="any">
              <ZSelect v-model="speedLimitUnit" :options="UNIT_OPTS" />
            </div>
          </div>
          <div class="field">
            <label>校验和(如 md5:xxxx)</label>
            <input type="text" v-model="checksum" placeholder="md5: / sha1: / sha256: / crc32:">
          </div>
        </div>
        <div class="field">
          <label>代理</label>
          <ZSelect v-model="proxy" :options="PROXY_OPTS" />
        </div>
        <div class="field" v-show="proxy === 'custom'">
          <label>代理地址(http(s):// 或 socks5://)</label>
          <input type="text" v-model="proxyCustom" placeholder="socks5://127.0.0.1:1080">
        </div>
      </div>
      <div class="dialog-actions">
        <button class="btn btn-ghost" @click="closeOverlay('newTaskOverlay')">取消</button>
        <button class="btn btn-primary" id="ntSubmit" @click="submit">开始下载</button>
      </div>
    </div>
  </Overlay>
</template>
