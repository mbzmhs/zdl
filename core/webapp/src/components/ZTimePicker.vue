<script setup>
// 自定义时间选择器,替代原生 <input type="time">:macOS WKWebView 里原生
// 控件渲染成无样式白底小方块。触发钮显示 HH:MM + 时钟图标;弹层为时/分两列
// 滚动选择,打开时自动滚到当前值,点时即改、点分后收起;支持跨天时段调用方
// 自行处理(本组件只管 HH:MM)。Esc/外点/滚动关闭。
import { ref, computed, nextTick, onBeforeUnmount } from "vue";

const props = defineProps({
  modelValue: { type: String, default: "00:00" },
});
const emit = defineEmits(["update:modelValue", "change"]);

const root = ref(null);
const pop = ref(null);
const hourCol = ref(null);
const minCol = ref(null);
const open = ref(false);

const HOURS = Array.from({ length: 24 }, (_, i) => i);
const MINUTES = Array.from({ length: 60 }, (_, i) => i);

const parts = computed(() => {
  const m = /^(\d{1,2}):(\d{1,2})/.exec(props.modelValue || "");
  const h = m ? Math.min(23, parseInt(m[1], 10)) : 0;
  const mi = m ? Math.min(59, parseInt(m[2], 10)) : 0;
  return { h, m: mi };
});
const display = computed(() =>
  `${String(parts.value.h).padStart(2, "0")}:${String(parts.value.m).padStart(2, "0")}`
);

const pad = (n) => String(n).padStart(2, "0");

function toggle() {
  open.value ? close() : show();
}

async function show() {
  open.value = true;
  await nextTick();
  const r = root.value?.getBoundingClientRect();
  const p = pop.value;
  if (!r || !p) return;
  const h = p.offsetHeight;
  let top = r.bottom + 5;
  if (top + h > window.innerHeight - 8 && r.top - h - 5 > 8) top = r.top - h - 5;
  p.style.left = r.left + "px";
  p.style.top = top + "px";
  hourCol.value?.scrollTo({ top: Math.max(0, parts.value.h * 30 - 90) });
  minCol.value?.scrollTo({ top: Math.max(0, parts.value.m * 30 - 90) });
  document.addEventListener("mousedown", onOutside, true);
  document.addEventListener("keydown", onKey, true);
  window.addEventListener("scroll", onScroll, true);
}

function close() {
  if (!open.value) return;
  open.value = false;
  document.removeEventListener("mousedown", onOutside, true);
  document.removeEventListener("keydown", onKey, true);
  window.removeEventListener("scroll", onScroll, true);
}

// 捕获阶段能收到时/分列自身的滚动事件,那是选时间的正常操作,不能关
function onScroll(e) {
  if (root.value?.contains(e.target) || pop.value?.contains(e.target)) return;
  close();
}

function onOutside(e) {
  if (!root.value?.contains(e.target) && !pop.value?.contains(e.target)) close();
}

function onKey(e) {
  if (e.key === "Escape") { e.stopPropagation(); close(); }
}

function setH(h) {
  emit("update:modelValue", `${pad(h)}:${pad(parts.value.m)}`);
  emit("change", display.value);
}
function setM(mi) {
  emit("update:modelValue", `${pad(parts.value.h)}:${pad(mi)}`);
  emit("change", display.value);
  close();
}

onBeforeUnmount(close);
</script>

<template>
  <div class="ztime" ref="root">
    <button type="button" class="ztime-trigger" :class="{ open }" @click="toggle" @keydown.enter.prevent="toggle">
      <svg class="ztime-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3.5 2"/></svg>
      <span class="ztime-value">{{ display }}</span>
    </button>
    <Teleport to="body">
      <div v-if="open" class="ztime-pop" ref="pop">
        <div class="ztime-head">
          <span>时间</span>
          <span class="ztime-value">{{ display }}</span>
        </div>
        <div class="ztime-cols">
          <div class="ztime-col" ref="hourCol">
            <div v-for="h in HOURS" :key="'h' + h" class="ztime-col-item" :class="{ selected: h === parts.h }" @click="setH(h)">{{ pad(h) }} 时</div>
          </div>
          <div class="ztime-col" ref="minCol">
            <div v-for="mi in MINUTES" :key="'m' + mi" class="ztime-col-item" :class="{ selected: mi === parts.m }" @click="setM(mi)">{{ pad(mi) }} 分</div>
          </div>
        </div>
      </div>
    </Teleport>
  </div>
</template>
