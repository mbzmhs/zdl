<script setup>
// 全局 toast:store.toasts 驱动;4200ms 淡出、4600ms 移除(时序在 store.toast 内)。
import { store } from "../store.js";
</script>

<template>
  <div class="toasts" id="toasts">
    <div v-for="t in store.toasts" :key="t.id" class="toast" :class="t.cls"
      :style="t.fading ? { opacity: 0, transition: 'opacity .3s' } : {}">{{ t.msg }}</div>
  </div>
</template>
