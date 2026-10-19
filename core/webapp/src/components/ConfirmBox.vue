<script setup>
// 全局确认弹窗(旧 #confirmOverlay):store.confirm 单例驱动。
// 关闭行为对齐旧 overlay 通用逻辑:click 派发在按下/松开位置的公共祖先上,
// 在对话框内按下、拖到遮罩上松开也会产生 target===ov 的 click,
// 因此要求按下也发生在遮罩上才关闭。二级危险按钮插在确认按钮之前。
import { computed, onMounted, onBeforeUnmount } from "vue";
import { store } from "../store.js";

const c = computed(() => store.confirm);

function close() { store.confirm = null; }

function onPointerDown(e) { downOnOv = e.target === e.currentTarget; }
let downOnOv = false;
function onClick(e) {
  if (e.target === e.currentTarget && downOnOv) close();
}
function onOk() {
  const cb = c.value?.onOk;
  close();
  cb && cb();
}
function onSecondary() {
  const cb = c.value?.onSecondary;
  close();
  cb && cb();
}
// Escape 关最上层弹窗:迁移期仅有确认弹窗,后续对话框批次并入统一管理
function onKey(e) {
  if (e.key === "Escape" && store.confirm) close();
}
onMounted(() => document.addEventListener("keydown", onKey));
onBeforeUnmount(() => document.removeEventListener("keydown", onKey));
</script>

<template>
  <div class="overlay" id="confirmOverlay" :class="{ show: !!c }"
    @pointerdown="onPointerDown" @click="onClick">
    <div class="dialog" style="width:min(420px,calc(100vw - 48px))">
      <h2><span class="accent-bar"></span><span>{{ c?.title || "确认操作" }}</span></h2>
      <p style="color:var(--text-dim);font-size:13.5px">{{ c?.message }}</p>
      <div class="dialog-actions">
        <button class="btn btn-ghost" @click="close">取消</button>
        <button v-if="c?.secondaryLabel" class="btn btn-danger" @click="onSecondary">{{ c.secondaryLabel }}</button>
        <button class="btn btn-primary" style="background:linear-gradient(90deg,#f87171,#ef4444)" @click="onOk">{{ c?.okLabel || "确认" }}</button>
      </div>
    </div>
  </div>
</template>
