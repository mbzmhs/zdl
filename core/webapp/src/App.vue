<script setup>
// 应用外壳:DOM 结构逐位对齐旧 index.html 的 header.topbar + div.layout。
import { onMounted, onBeforeUnmount } from "vue";
import TopBar from "./components/TopBar.vue";
import SideBar from "./components/SideBar.vue";
import TaskList from "./components/TaskList.vue";
import Toasts from "./components/Toasts.vue";
import ConfirmBox from "./components/ConfirmBox.vue";
import NewTaskDialog from "./components/NewTaskDialog.vue";
import BTDialog from "./components/BTDialog.vue";
import ChooseDialog from "./components/ChooseDialog.vue";
import Drawer from "./components/Drawer.vue";
import PlayerPane from "./components/PlayerPane.vue";
import SettingsDialog from "./components/SettingsDialog.vue";
import RssPane from "./components/RssPane.vue";
import RssDialogs from "./components/RssDialogs.vue";
import CtxMenu from "./components/CtxMenu.vue";
import { store, ui, closeTopOverlay } from "./store.js";
import { hideCtxMenu } from "./lib/ctxmenu.js";

// Escape:收右键菜单 + 关最上层弹窗(DOM 序语义见 store.closeTopOverlay)
function onKey(e) {
  if (e.key !== "Escape") return;
  hideCtxMenu();
  if (ui.stack.length) closeTopOverlay();
}
onMounted(() => document.addEventListener("keydown", onKey));
onBeforeUnmount(() => document.removeEventListener("keydown", onKey));
</script>

<template>
  <TopBar />
  <div class="layout">
    <SideBar />
    <main class="main">
      <TaskList />
      <RssPane v-show="store.currentView === 'rss'" />
    </main>
  </div>
  <NewTaskDialog />
  <BTDialog />
  <ChooseDialog />
  <Drawer />
  <PlayerPane />
  <SettingsDialog />
  <RssDialogs />
  <ConfirmBox />
  <CtxMenu />
  <Toasts />
</template>
