<script setup>
// 顶栏:品牌/速度部件(曲线+数字)/连接徽章/主题/设置/新建任务入口。
// DOM 与旧 index.html 的 header.topbar 一致;#speedChart/#themeBtn/#settingsBtn
// 的 id 被 style.css 选择器引用,不可改名。
import { ref, computed, onMounted, onBeforeUnmount } from "vue";
import { store, applyTheme } from "../store.js";
import { fmtSpeed } from "../lib/fmt.js";
import SpeedChart from "./SpeedChart.vue";
import appIcon from "../assets/app-icon.png";

// 新建任务下拉:caret 切换,点击页面任意处收起(对齐旧 initCreateMenu)
const menuOpen = ref(false);
function toggleMenu(e) { e.stopPropagation(); menuOpen.value = !menuOpen.value; }
function menuPick(mode) {
  menuOpen.value = false;
  if (mode === "bt") store.actions.openBTDialog && store.actions.openBTDialog();
  else store.actions.openNewTask && store.actions.openNewTask();
}
onMounted(() => document.addEventListener("click", menuDocClose));
onBeforeUnmount(() => document.removeEventListener("click", menuDocClose));
function menuDocClose() { menuOpen.value = false; }

const isDark = computed(() => {
  void store.themeTick; // 主题切换计数变化时重读 DOM 状态
  return document.documentElement.dataset.theme === "dark";
});
// 旧 initTheme:两种状态各自替换 #themeIcon 的内部路径
const themeIconPath = computed(() => isDark.value
  ? '<path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z" fill="currentColor" stroke="none"/>'
  : '<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>');

function toggleTheme() {
  applyTheme(isDark.value ? "light" : "dark");
}
</script>

<template>
  <header class="topbar">
    <div class="brand">
      <div class="logo"><img :src="appIcon" alt="ZDownloader"></div>
      <div class="name">ZDownloader<small id="verBadge">v{{ store.version || "?" }}</small></div>
    </div>

    <div class="speed-widget">
      <SpeedChart />
      <div class="speed-number">
        <div class="sp-row"><i class="dot down"></i><span class="l">下载</span><span class="v">{{ fmtSpeed(store.stats.totalSpeed) }}</span></div>
        <div class="sp-row"><i class="dot up"></i><span class="l">上传</span><span class="v">{{ fmtSpeed(store.stats.totalUpSpeed || 0) }}</span></div>
      </div>
      <div class="conn-badge" id="connBadge" :class="{ off: !store.conn }">{{ store.conn ? "已连接" : "连接断开,重连中…" }}</div>
      <button class="icon-btn" id="themeBtn" title="切换主题" @click="toggleTheme">
        <svg id="themeIcon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" v-html="themeIconPath"></svg>
      </button>
      <button class="btn btn-ghost" id="settingsBtn" title="设置" @click="store.actions.openSettings && store.actions.openSettings()">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>
      </button>
      <div class="btn-split">
        <button class="btn btn-primary" id="newTaskBtn" @click="store.actions.openNewTask && store.actions.openNewTask()">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"><path d="M12 5v14M5 12h14"/></svg>
          新建任务
        </button>
        <button class="btn btn-primary btn-caret" id="newTaskCaret" title="选择任务类型" aria-label="选择任务类型" @click="toggleMenu">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round"><path d="M6 9l6 6 6-6"/></svg>
        </button>
        <div class="dropdown-menu" id="newTaskMenu" v-show="menuOpen">
          <button type="button" data-mode="link" @click="menuPick('link')">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/></svg>
            链接任务(HTTP / HLS)
          </button>
          <button type="button" data-mode="bt" @click="menuPick('bt')">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2a10 10 0 1 0 10 10"/><path d="M12 7a5 5 0 1 0 5 5"/><circle cx="12" cy="12" r="1" fill="currentColor"/></svg>
            磁力 / 种子任务(BT)
          </button>
        </div>
      </div>
    </div>
  </header>
</template>
