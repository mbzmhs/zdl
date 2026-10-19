<script setup>
// peer 分块图:每 piece 一竖列,列宽不足时多 piece 合并(全有=实蓝,部分=半蓝)。
// 逻辑逐行对齐旧 drawBitfields 单画布循环,拆为每行一个自绘组件。
import { ref, onMounted, watch } from "vue";

const props = defineProps({ bf: { type: String, default: "" } });
const cv = ref(null);

function draw() {
  const canvas = cv.value;
  if (!canvas) return;
  const ctx = canvas.getContext("2d");
  const w = canvas.width, h = canvas.height;
  ctx.clearRect(0, 0, w, h);
  if (!props.bf) return;
  let raw;
  try { raw = atob(props.bf); } catch { return; }
  const n = raw.length * 8;
  for (let x = 0; x < w; x++) {
    const s = Math.floor(x * n / w), e = Math.max(s + 1, Math.floor((x + 1) * n / w));
    let any = false, full = true;
    for (let i = s; i < e && i < n; i++) {
      const bit = (raw.charCodeAt(i >> 3) >> (7 - (i & 7))) & 1;
      if (bit) any = true; else full = false;
    }
    if (any) {
      ctx.fillStyle = full ? "#3b82f6" : "rgba(59,130,246,0.45)";
      ctx.fillRect(x, 1, 1, h - 2);
    }
  }
}
onMounted(draw);
watch(() => props.bf, draw);
</script>

<template>
  <canvas ref="cv" class="bf-canvas" width="110" height="13"></canvas>
</template>
