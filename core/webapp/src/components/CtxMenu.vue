<script setup>
// 右键菜单:popover=manual 顶层渲染;显示后置零测量再钳位到视口内。
// pointerdown 在菜单外 / blur / resize / wheel 即收起(与旧监听一致)。
import { ref, watch, nextTick, onMounted, onBeforeUnmount } from "vue";
import { ctxState, hideCtxMenu } from "../lib/ctxmenu.js";

const el = ref(null);

watch(() => ctxState.visible, async v => {
  if (!v) {
    if (el.value?.matches(":popover-open")) el.value.hidePopover();
    return;
  }
  await nextTick();
  const m = el.value;
  m.showPopover();
  m.style.left = "0px"; m.style.top = "0px"; // 先置零再测量,避免旧位置导致测量偏大
  const r = m.getBoundingClientRect();
  m.style.left = Math.max(4, Math.min(ctxState.x, window.innerWidth - r.width - 8)) + "px";
  m.style.top = Math.max(4, Math.min(ctxState.y, window.innerHeight - r.height - 8)) + "px";
});

function pick(it) {
  if (it.disabled) return;
  hideCtxMenu();
  it.action();
}
function onDocPointerDown(e) {
  if (!e.target.closest("#ctxMenu")) hideCtxMenu();
}
onMounted(() => {
  document.addEventListener("pointerdown", onDocPointerDown, true);
  window.addEventListener("blur", hideCtxMenu);
  window.addEventListener("resize", hideCtxMenu);
  window.addEventListener("wheel", hideCtxMenu, { passive: true });
});
onBeforeUnmount(() => {
  document.removeEventListener("pointerdown", onDocPointerDown, true);
  window.removeEventListener("blur", hideCtxMenu);
  window.removeEventListener("resize", hideCtxMenu);
  window.removeEventListener("wheel", hideCtxMenu);
});
</script>

<template>
  <div id="ctxMenu" ref="el" class="ctx-menu" popover="manual">
    <template v-for="(it, i) in ctxState.items" :key="i">
      <div v-if="it === '-'" class="ctx-sep"></div>
      <div v-else class="ctx-item" :class="{ disabled: it.disabled }" @click="pick(it)">{{ it.label }}</div>
    </template>
  </div>
</template>
