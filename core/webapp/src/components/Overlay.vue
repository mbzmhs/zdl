<script setup>
// 通用模态遮罩:显隐由 store 的 overlay 栈驱动(id 必须在 OVERLAY_RANK 注册)。
// 关闭行为对齐旧 overlay 通用逻辑:click 派发在按下/松开位置的公共祖先上,
// 在对话框内按下、拖到遮罩上松开也会产生 target===ov 的 click,
// 因此要求按下也发生在遮罩上才关闭。data-close 按钮直接调 closeOverlay。
import { ui, closeOverlay } from "../store.js";

const props = defineProps({ id: { type: String, required: true } });
const emit = defineEmits(["mask-close"]);

let downOnOv = false;
function onPointerDown(e) { downOnOv = e.target === e.currentTarget; }
function onClick(e) {
  if (e.target === e.currentTarget && downOnOv) {
    emit("mask-close");
    closeOverlay(props.id);
  }
}
</script>

<template>
  <div class="overlay" :id="id" :class="{ show: !!ui.flags[id] }"
    @pointerdown="onPointerDown" @click="onClick">
    <slot />
  </div>
</template>
