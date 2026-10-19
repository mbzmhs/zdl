<script setup>
// 顶栏总速度曲线:canvas 绘制逻辑逐行对齐旧 drawChart()。
// 重绘节拍 = 快照被接受(store.snapTick);主题切换重读 CSS 变量后重绘(themeTick)。
import { ref, watch, onMounted } from "vue";
import { store, speedHistory, upSpeedHistory } from "../store.js";

const cv = ref(null);
let ctx = null;

onMounted(() => {
  ctx = cv.value.getContext("2d");
  draw();
});

function draw() {
  if (!ctx) return;
  const w = cv.value.width, h = cv.value.height;
  ctx.setTransform(1, 0, 0, 1, 0, 0);
  ctx.clearRect(0, 0, w, h);
  const max = Math.max(...speedHistory, ...upSpeedHistory, 1024) * 1.15;
  const n = speedHistory.length;
  if (n < 2) return;

  // 折线 + 底部渐隐填充(叠层与详情速度分页一致:上传在下、下载在上)
  const line = (hist, stroke, fillTop, fillBottom) => {
    ctx.beginPath();
    for (let i = 0; i < n; i++) {
      const x = (i / (n - 1)) * (w - 6) + 3;
      const y = h - 4 - (hist[i] / max) * (h - 10);
      i ? ctx.lineTo(x, y) : ctx.moveTo(x, y);
    }
    ctx.strokeStyle = stroke;
    ctx.lineWidth = 2.2;
    ctx.lineJoin = "round";
    ctx.stroke();

    ctx.lineTo(w - 3, h);
    ctx.lineTo(3, h);
    ctx.closePath();
    const fill = ctx.createLinearGradient(0, 0, 0, h);
    fill.addColorStop(0, fillTop);
    fill.addColorStop(1, fillBottom);
    ctx.fillStyle = fill;
    ctx.fill();
  };

  const grad = ctx.createLinearGradient(0, 0, w, 0);
  grad.addColorStop(0, "rgba(79,140,255,0.15)");
  grad.addColorStop(1, getComputedStyle(document.body).getPropertyValue("--speed-curve").trim() || "rgba(79,140,255,0.9)");
  line(upSpeedHistory, "#34d399", "rgba(52,211,153,0.14)", "rgba(52,211,153,0)"); // 上传
  line(speedHistory, grad, "rgba(79,140,255,0.18)", "rgba(79,140,255,0)");        // 下载(上层)
}

watch(() => store.snapTick, draw);
watch(() => store.themeTick, () => requestAnimationFrame(draw));
</script>

<template>
  <canvas id="speedChart" ref="cv" width="380" height="80"></canvas>
</template>
