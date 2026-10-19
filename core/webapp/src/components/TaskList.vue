<script setup>
// 任务列表:展示按创建时间倒序(最新在最上);视图切换重触发 view-swap 淡入动画。
// filter 生成新数组,sort 就地排它不影响 store.tasks(与旧 render() 同口径)。
import { ref, computed, watch, nextTick } from "vue";
import { store, viewFilter } from "../store.js";
import TaskCard from "./TaskCard.vue";

const listEl = ref(null);

const visible = computed(() =>
  store.tasks.filter(t => viewFilter(t.status))
    .sort((a, b) => new Date(b.createdAt) - new Date(a.createdAt)));

const isRss = computed(() => store.currentView === "rss");
const emptyText = computed(() => ({
  all: "还没有任何任务,点击右上角「新建任务」开始",
  active: "暂无下载任务,点击右上角「新建任务」开始",
  completed: "还没有已完成的任务",
  trash: "已取消的任务会显示在这里,可随时重新下载",
  seeding: "没有正在做种的任务;完成任务后按 BT 设置中的策略自动做种",
}[store.currentView]));

// 内容区淡入上浮:重触发动画(移除→强制回流→再加回,与旧 switchView 同法)
watch(() => store.viewTick, () => {
  nextTick(() => {
    const tl = listEl.value;
    if (!tl) return;
    tl.classList.remove("view-swap");
    void tl.offsetWidth;
    tl.classList.add("view-swap");
  });
});
</script>

<template>
  <div class="task-list" id="taskList" ref="listEl" v-show="!isRss">
    <TaskCard v-for="t in visible" :key="t.id" :t="t" />
  </div>
  <div class="empty" id="emptyState" v-show="!visible.length && !isRss">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5 5 5-5M12 15V3"/></svg>
    <div class="hint">{{ emptyText }}</div>
  </div>
</template>
