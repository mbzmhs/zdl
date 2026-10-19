<script setup>
// 侧栏导航:分类计数 + 滑动指示条(几何测量逻辑逐行对齐旧 makeSlideInd)。
// DOM 与旧 index.html 的 nav.sidebar 一致(.nav-slide-ind 由旧版 JS 前插,此处模板化)。
import { ref, computed, watch, onMounted, onBeforeUnmount, nextTick } from "vue";
import { store, switchView, clearView } from "../store.js";

const nav = ref(null);

const count = st => store.tasks.filter(t => t.status === st).length;
const countAll = computed(() => store.tasks.length);
const countActive = computed(() =>
  count("running") + count("waiting") + count("paused") + count("error") + count("choosing"));
const countCompleted = computed(() => count("completed"));
const countTrash = computed(() => count("canceled"));
const countSeeding = computed(() => count("seeding"));
// 旧 rssBadge(n):>0 显示数字,否则整枚徽章 display:none
const rssUnread = computed(() => store.stats.rssUnread || 0);

// 侧栏滑动指示条:高亮元素插在导航容器里,切换时 transform 平移到新激活项
// (宿主:包着清扫按钮/徽章的行容器,或按钮本身;几何取自 offsetParent=容器)
let ind = null;
function syncInd() {
  const act = nav.value?.querySelector(".nav-item.active");
  if (!ind) return;
  if (!act) { ind.style.opacity = "0"; return; }
  const host = act.closest(".nav-item-wrap") || act;
  ind.style.opacity = "1";
  ind.style.width = host.offsetWidth + "px";
  ind.style.height = host.offsetHeight + "px";
  ind.style.transform = `translate(${host.offsetLeft}px, ${host.offsetTop}px)`;
}
onMounted(() => {
  ind = nav.value.querySelector(".nav-slide-ind");
  syncInd();
  window.addEventListener("resize", syncInd);
});
onBeforeUnmount(() => window.removeEventListener("resize", syncInd));
watch(() => store.currentView, () => nextTick(syncInd));
</script>

<template>
  <nav class="sidebar" ref="nav">
    <span class="nav-slide-ind"></span>
    <button class="nav-item" :class="{ active: store.currentView === 'all' }" @click="switchView('all')">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 6h16M4 12h16M4 18h16"/></svg>
      全部 <span class="count">{{ countAll }}</span>
    </button>
    <button class="nav-item" :class="{ active: store.currentView === 'active' }" @click="switchView('active')">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5 5 5-5M12 15V3"/></svg>
      下载中 <span class="count">{{ countActive }}</span>
    </button>
    <button class="nav-item" :class="{ active: store.currentView === 'seeding' }" @click="switchView('seeding')">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3v10"/><path d="M8 7l4-4 4 4"/><path d="M4 15v3a3 3 0 0 0 3 3h10a3 3 0 0 0 3-3v-3"/></svg>
      做种中 <span class="count">{{ countSeeding }}</span>
    </button>
    <div class="nav-item-wrap">
      <button class="nav-item" :class="{ active: store.currentView === 'completed' }" @click="switchView('completed')">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="3" width="18" height="18" rx="3"/><path d="M8.5 12.5l2.5 2.5 5-6"/></svg>
        已完成
      </button>
      <button type="button" class="nav-clear" title="清扫该分类的全部任务" @click="clearView('completed', '已完成')">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m7 21-4.3-4.3c-1-1-1-2.5 0-3.4l9.6-9.6c1-1 2.5-1 3.4 0l5.6 5.6c1 1 1 2.5 0 3.4L13 21"/><path d="M22 21H7"/><path d="m5 11 9 9"/></svg>
      </button>
      <span class="count">{{ countCompleted }}</span>
    </div>
    <div class="nav-item-wrap">
      <button class="nav-item" :class="{ active: store.currentView === 'trash' }" @click="switchView('trash')">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18M8 6V4a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v2M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6M10 11v6M14 11v6"/></svg>
        已取消
      </button>
      <button type="button" class="nav-clear" title="清扫该分类的全部任务" @click="clearView('trash', '已取消')">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m7 21-4.3-4.3c-1-1-1-2.5 0-3.4l9.6-9.6c1-1 2.5-1 3.4 0l5.6 5.6c1 1 1 2.5 0 3.4L13 21"/><path d="M22 21H7"/><path d="m5 11 9 9"/></svg>
      </button>
      <span class="count">{{ countTrash }}</span>
    </div>
    <div class="nav-item-wrap">
      <button class="nav-item" :class="{ active: store.currentView === 'rss' }" @click="switchView('rss')">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M4 11a9 9 0 0 1 9 9"/><path d="M4 4a16 16 0 0 1 16 16"/><circle cx="5" cy="19" r="1.6" fill="currentColor" stroke="none"/></svg>
        RSS订阅
      </button>
      <span class="count" :style="{ display: rssUnread > 0 ? '' : 'none' }">{{ rssUnread > 0 ? rssUnread : "" }}</span>
    </div>
    <div class="spacer"></div>
  </nav>
</template>
