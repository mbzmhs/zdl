<script setup>
// RSS 三弹窗:添加/编辑订阅、新建/重命名文件夹、条目页面预览(iframe 内嵌)。
// DOM 对齐旧 #rssFeedOverlay / #rssFolderOverlay / #rssPrevOverlay。
import { ref, reactive, computed, watch, nextTick, onMounted } from "vue";
import { api } from "../lib/api.js";
import { store, ui, openOverlay, closeOverlay, toast, showErr, pickDir } from "../store.js";
import { rss, rssFeedDlg, rssFolderDlg, rssLoad } from "../rss.js";
import Overlay from "./Overlay.vue";
import ZSelect from "./ZSelect.vue";

const FOLDER_OPTS = computed(() => [
  { value: "", label: "未分组" },
  ...rss.tree.folders.map((fo) => ({ value: fo.id, label: fo.name })),
]);
const AUTODL_OPTS = [
  { value: "inherit", label: "继承全局" },
  { value: "off", label: "不自动下载" },
  { value: "notify", label: "自动下载(完成后提醒)" },
  { value: "silent", label: "静默下载" },
];

/* ---------- 添加 / 编辑订阅 ---------- */
const feedUrl = ref(""), feedName = ref(""), feedFolder = ref("");
const feedAutoDl = ref("inherit"), feedDir = ref("");
const feedProbe = reactive({ show: false, cls: "probe-line", text: "" });

watch(() => ui.flags.rssFeedOverlay, v => {
  if (!v) return;
  const edit = rssFeedDlg.edit;
  feedUrl.value = edit ? edit.url : "";
  feedName.value = edit ? (edit.title || "") : "";
  feedFolder.value = edit ? (edit.folderId || "") : (rssFeedDlg.folderId || "");
  feedAutoDl.value = edit && ["off", "notify", "silent"].includes(edit.autoDl) ? edit.autoDl : "inherit";
  feedDir.value = edit ? (edit.downloadDir || "") : "";
  feedProbe.show = false;
});

async function feedSubmit() {
  const url = feedUrl.value.trim();
  const title = feedName.value.trim();
  const folderId = feedFolder.value;
  const autoDl = feedAutoDl.value;
  const downloadDir = feedDir.value.trim();
  if (rssFeedDlg.editId) {
    try {
      await api("PUT", "/api/rss/feeds/" + rssFeedDlg.editId, { title, folderId, autoDl, downloadDir });
    } catch (e) { showErr(e); return; }
    closeOverlay("rssFeedOverlay");
    toast("订阅已保存", "ok");
    rssLoad();
    return;
  }
  if (!url) { toast("请输入订阅地址", "err"); return; }
  try {
    await api("POST", "/api/rss/feeds", { url, folderId, title, autoDl, downloadDir });
  } catch (e) {
    feedProbe.cls = "probe-line bad";
    feedProbe.text = "✗ " + e.message;
    feedProbe.show = true;
    return;
  }
  closeOverlay("rssFeedOverlay");
  toast("订阅已添加,正在抓取…", "ok");
  rssLoad();
}
async function feedBrowse() {
  try {
    const r = await pickDir("选择订阅下载目录");
    if (r.ok && r.path) feedDir.value = r.path;
  } catch (_) {}
}

/* ---------- 新建 / 重命名文件夹 ---------- */
const folderName = ref("");
watch(() => ui.flags.rssFolderOverlay, v => {
  if (!v) return;
  const fo = (rss.tree.folders || []).find(f => f.id === rssFolderDlg.editId);
  folderName.value = fo ? fo.name : "";
});
async function folderSubmit() {
  const name = folderName.value.trim();
  if (!name) { toast("请输入文件夹名称", "err"); return; }
  try {
    if (rssFolderDlg.editId) await api("PUT", "/api/rss/folders/" + rssFolderDlg.editId, { name });
    else await api("POST", "/api/rss/folders", { name });
  } catch (e) { showErr(e); return; }
  closeOverlay("rssFolderOverlay");
  toast(rssFolderDlg.editId ? "文件夹已重命名" : "文件夹已创建", "ok");
  rssLoad();
}

/* ---------- 页面预览 ---------- */
// iframe 仅在弹窗打开期间渲染(v-if):隐藏态的 iframe 是独立原生图层,WebKit
// 不吃 host 页的 pointer-events:none,悬停其区域的滚轮会被路由进 iframe 文档
// 蒸发,父页面连事件都收不到——曾致全 App 内容区滚动失效(实测 Safari 同症)。
const prevTitle = ref("页面预览"), prevUrl = ref("");
const frameEl = ref(null);
let frameOrig = "";
function openRssPreview(url, name) {
  prevTitle.value = name || "页面预览";
  prevUrl.value = url;
  frameOrig = url;
  openOverlay("rssPrevOverlay");
  nextTick(() => { if (frameEl.value) frameEl.value.src = url; });
}
async function prevCopy() {
  try {
    await navigator.clipboard.writeText(frameOrig);
    toast("链接已复制", "ok");
  } catch (_) { toast("复制失败,可手动选中地址复制", "err"); }
}
function prevOpen() {
  if (frameOrig) window.open(frameOrig, "_blank", "noopener");
}

onMounted(() => {
  store.actions.openRssPreview = openRssPreview;
});
</script>

<template>
  <!-- 添加 / 编辑 RSS 订阅 -->
  <Overlay id="rssFeedOverlay">
    <div class="dialog" style="width:min(560px,calc(100vw - 48px))">
      <h2><span class="accent-bar"></span><span>{{ rssFeedDlg.editId ? "编辑 RSS 订阅" : "添加 RSS 订阅" }}</span></h2>
      <div class="field">
        <label>订阅地址(RSS 2.0 / RSS 1.0 / Atom)</label>
        <input type="text" id="rssFeedUrl" v-model="feedUrl" placeholder="https://example.com/feed.xml"
          spellcheck="false" :disabled="!!rssFeedDlg.editId"
          @keydown.enter="feedSubmit">
        <div v-show="feedProbe.show" :class="feedProbe.cls">{{ feedProbe.text }}</div>
      </div>
      <div class="field-row">
        <div class="field">
          <label>名称(留空用站点标题)</label>
          <input type="text" v-model="feedName" placeholder="自动识别">
        </div>
        <div class="field">
          <label>文件夹</label>
          <ZSelect v-model="feedFolder" :options="FOLDER_OPTS" />
        </div>
      </div>
      <div class="field">
        <label>自动下载</label>
        <ZSelect v-model="feedAutoDl" :options="AUTODL_OPTS" />
      </div>
      <div class="field">
        <label>下载目录(留空用全局默认)</label>
        <div class="input-row">
          <input type="text" v-model="feedDir" placeholder="默认下载目录" spellcheck="false">
          <button type="button" class="btn btn-ghost" id="rssFeedPickDir" @click="feedBrowse">浏览…</button>
        </div>
      </div>
      <div class="hint-text">添加后立即抓取一次;条目按设置的间隔自动更新(设置 → RSS 订阅)。自动下载只抓条目的资源链接(附件/磁力),不下载条目页面;触发后该条目置为已读。</div>
      <div class="dialog-actions">
        <button class="btn btn-ghost" @click="closeOverlay('rssFeedOverlay')">取消</button>
        <button class="btn btn-primary" id="rssFeedSubmit" @click="feedSubmit">{{ rssFeedDlg.editId ? "保存" : "添加" }}</button>
      </div>
    </div>
  </Overlay>

  <!-- 新建 / 重命名 RSS 文件夹 -->
  <Overlay id="rssFolderOverlay">
    <div class="dialog" style="width:min(440px,calc(100vw - 48px))">
      <h2><span class="accent-bar"></span><span>{{ rssFolderDlg.editId ? "重命名文件夹" : "新建文件夹" }}</span></h2>
      <div class="field">
        <label>文件夹名称</label>
        <input type="text" id="rssFolderName" v-model="folderName" placeholder="例如:影视 / 动漫 / 音乐" @keydown.enter="folderSubmit">
      </div>
      <div class="dialog-actions">
        <button class="btn btn-ghost" @click="closeOverlay('rssFolderOverlay')">取消</button>
        <button class="btn btn-primary" id="rssFolderSubmit" @click="folderSubmit">确定</button>
      </div>
    </div>
  </Overlay>

  <!-- RSS 条目页面预览(直接内嵌原页面;禁止内嵌/需验证的站点走新窗口) -->
  <Overlay id="rssPrevOverlay">
    <div class="dialog prev-dialog">
      <h2><span class="accent-bar"></span><span>{{ prevTitle }}</span></h2>
      <div class="prev-url" :title="prevUrl">{{ prevUrl }}</div>
      <div class="prev-frame"><iframe v-if="ui.flags.rssPrevOverlay" id="rssPrevFrame" ref="frameEl" title="页面预览"
        sandbox="allow-same-origin allow-scripts allow-popups allow-forms"
        referrerpolicy="no-referrer"></iframe></div>
      <div class="hint-text">页面由站点直接提供;若显示空白或验证码,说明该站禁止被内嵌或需要人工验证,请改用「在新窗口打开」。</div>
      <div class="dialog-actions">
        <button class="btn btn-ghost" id="rssPrevCopy" @click="prevCopy">复制链接</button>
        <button class="btn btn-ghost" id="rssPrevOpen" @click="prevOpen">在新窗口打开</button>
        <button class="btn btn-primary" @click="closeOverlay('rssPrevOverlay')">关闭</button>
      </div>
    </div>
  </Overlay>
</template>
