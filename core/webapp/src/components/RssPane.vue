<script setup>
// RSS 订阅面板:文件夹/订阅/条目/资源链接四级树 + 拖拽排序 + 批量勾选吸底条。
// 交互语义逐条对齐旧 initRss 委托:按钮优先,链接行点击切换复选框,
// 折叠条目整行点击 = 勾选全选资源,展开走行首箭头。DOM 对齐旧 renderRss 输出。
import { ref, computed, watch, onMounted, onBeforeUnmount } from "vue";
import { store } from "../store.js";
import { ms } from "../lib/icons.js";
import { rss, rssLoad, rssAction, rssToggleFolder, rssToggleFeed, selClear, startRssPoll, stopRssPoll, makeRssDnd } from "../rss.js";
import RssFeedBlock from "./RssFeedBlock.vue";

const pane = ref(null);
let detachDnd = null;

const CARET_PATH = "M6 9l6 6 6-6";
const feedsOf = fid => (rss.tree.feeds || []).filter(f => (f.folderId || "") === fid)
  .sort((a, b) => (a.order || 0) - (b.order || 0));
const folders = computed(() => rss.tree.folders || []);
const feedsAll = computed(() => rss.tree.feeds || []);
const unreadAll = computed(() => rss.tree.unread || 0);
const selN = computed(() => rss.sel.size);
const folderUnread = id => feedsOf(id).reduce((s, f) => s + (f.unread || 0), 0);

function dropClsFolder(id) {
  const key = "F:" + id;
  if (rss.dropMark.key !== key) return {};
  return rss.dropMark.pos === "before" ? { "drop-before": true } : { "drop-after": true };
}

/* 整行点击语义(旧委托):
   - 按钮[data-rss] 优先
   - 资源链接行(非复选框/按钮)→ 切换该行复选框(磁力行禁用不生效)
   - 条目行(非展开箭头/链接区)→ 折叠态等同点"全选资源"复选框
   - 订阅行(非条目区)→ 展开/收起(RssFeedBlock 内箭头走 data-rss)
   - 文件夹行 → 折叠/展开 */
function onPaneClick(e) {
  const act = e.target.closest("[data-rss]");
  if (act) { rssAction(act.dataset.rss, act); return; }
  const linkRow = e.target.closest(".rss-link");
  if (linkRow) {
    const cb = linkRow.querySelector('input[data-rss="sel-link"]');
    if (cb && !cb.disabled) { cb.checked = !cb.checked; cb.dispatchEvent(new Event("change")); }
    return;
  }
  const item = e.target.closest(".rss-item");
  if (item && !e.target.closest(".rss-links")) {
    const cb = item.querySelector('input[data-rss="sel-item"]');
    if (cb) { cb.checked = !cb.checked; cb.dispatchEvent(new Event("change")); }
    return;
  }
  const feed = e.target.closest(".rss-feed");
  if (feed) {
    // 订阅行主体点击 = 展开/收起(整行语义,旧版同款)
    rssToggleFeed(feed.dataset.feed);
    return;
  }
  const fo = e.target.closest(".rss-folder");
  if (fo) rssToggleFolder(fo.dataset.folder);
}

watch(() => store.currentView, v => {
  if (v === "rss") {
    rssLoad();
    startRssPoll();
  } else {
    stopRssPoll();
  }
});
onMounted(() => {
  detachDnd = makeRssDnd(pane.value);
  if (store.currentView === "rss") { rssLoad(); startRssPoll(); }
});
onBeforeUnmount(() => { detachDnd && detachDnd(); stopRssPoll(); });
</script>

<template>
  <div class="rss-pane" id="rssPane" ref="pane" :class="{ 'has-selbar': selN > 0 }" @click="onPaneClick">
    <div class="rss-head">
      <div class="rss-head-title">RSS 订阅
        <span class="dim">{{ folders.length }} 个文件夹 · {{ feedsAll.length }} 个订阅 · {{ unreadAll }} 条未读</span>
      </div>
      <div class="rss-head-acts">
        <button class="btn btn-ghost" data-rss="newfolder">新建文件夹</button>
        <button class="btn btn-ghost" data-rss="readall" :disabled="!unreadAll">全部已读</button>
        <button class="btn btn-ghost" data-rss="refreshall">{{ rss.tree.busy ? "更新中…" : "全部更新" }}</button>
        <button class="btn btn-primary" data-rss="addfeed">添加订阅</button>
      </div>
    </div>
    <div class="rss-tree">
      <div v-if="!folders.length && !feedsAll.length" class="rss-empty">
        <div>还没有 RSS 订阅</div>
        <div class="s">点右上角「添加订阅」粘贴 RSS / Atom 地址;下载链接可一键创建任务,支持勾选批量下载</div>
      </div>
      <template v-for="fo in folders" :key="fo.id">
        <div class="rss-folder" :class="[{ collapsed: rss.collapsedFolders.has('F:' + fo.id) }, dropClsFolder(fo.id)]"
          draggable="true" data-drag="folder" :data-folder="fo.id" data-drop="folder">
          <span class="rss-caret" data-rss="toggle-folder" :data-folder="fo.id">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"><path :d="CARET_PATH"/></svg>
          </span>
          <span class="ms" v-html="ms(rss.collapsedFolders.has('F:' + fo.id) ? 'folder' : 'folder_open')"></span>
          <span class="n">{{ fo.name }}</span>
          <span v-if="folderUnread(fo.id) > 0" class="rss-badge">{{ folderUnread(fo.id) }}</span>
          <span class="cnt">{{ feedsOf(fo.id).length }} 个订阅</span>
          <span class="row-acts">
            <button class="icon-btn" data-rss="addfeed" :data-folder="fo.id" title="在此文件夹添加订阅" v-html="ms('add')"></button>
            <button class="icon-btn" data-rss="renamefolder" :data-folder="fo.id" title="重命名文件夹" v-html="ms('movie_edit')"></button>
            <button class="icon-btn" data-rss="delfolder" :data-folder="fo.id" title="删除文件夹(其中订阅移至未分组)" v-html="ms('delete')"></button>
          </span>
        </div>
        <div class="rss-children" v-show="!rss.collapsedFolders.has('F:' + fo.id)">
          <div v-if="!feedsOf(fo.id).length" class="rss-hint">该文件夹暂无订阅,可拖动订阅到此</div>
          <RssFeedBlock v-for="f in feedsOf(fo.id)" :key="f.id" :f="f" />
        </div>
      </template>
      <!-- 未分组(始终渲染:既是分组也是拖拽到根部的放置区) -->
      <div class="rss-folder rss-root" :class="dropClsFolder('')" data-drop="folder" data-folder="">
        <span class="rss-caret" data-rss="toggle-folder" data-folder="">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"><path :d="CARET_PATH"/></svg>
        </span>
        <span class="ms" v-html="ms('folder')"></span>
        <span class="n">未分组</span>
        <span v-if="folderUnread('') > 0" class="rss-badge">{{ folderUnread('') }}</span>
        <span class="cnt">{{ feedsOf('').length }} 个订阅</span>
      </div>
      <div class="rss-children" v-show="!rss.collapsedFolders.has('F:')">
        <div v-if="!feedsOf('').length" class="rss-hint">暂无未分组订阅</div>
        <RssFeedBlock v-for="f in feedsOf('')" :key="f.id" :f="f" />
      </div>
    </div>
    <!-- 已选条:置于内容末尾 + CSS sticky 吸底,滚动时始终可见 -->
    <div class="rss-selbar" id="rssSelBar" v-show="selN > 0">
      <span class="lbl">已选 <b>{{ selN }}</b> 个链接</span>
      <button class="btn btn-ghost btn-sm" data-rss="sel-clear">清空选择</button>
      <button class="btn btn-primary btn-sm" data-rss="sel-download">批量下载</button>
    </div>
  </div>
</template>
