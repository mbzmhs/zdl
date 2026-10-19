<script setup>
// 自定义下拉,替代原生 <select>:macOS WKWebView 里原生 select 渲染系统
// aqua 凹槽样式且弹出菜单是亮色 NSMenu,与暗色主题完全脱节(Windows 上
// WebView2 恰好还行)。触发布局对齐 .field input;弹层 Teleport 到 body
// (fixed 定位,z 200 压过对话框 95/右键菜单 120),滚动即关、Esc 关、外点关,
// ↑↓ 高亮 + Enter 选中。
import { ref, computed, nextTick, onBeforeUnmount } from "vue";

const props = defineProps({
  modelValue: { type: [String, Number], default: "" },
  options: { type: Array, default: () => [] }, // 字符串/数字 或 {value,label}
  disabled: { type: Boolean, default: false },
  placeholder: { type: String, default: "请选择" },
});
const emit = defineEmits(["update:modelValue", "change"]);

const root = ref(null);
const pop = ref(null);
const open = ref(false);
const popStyle = ref({});
const hi = ref(-1); // 键盘高亮下标

const normalized = computed(() =>
  props.options.map((o) =>
    o !== null && typeof o === "object" ? o : { value: o, label: String(o) }
  )
);
const current = computed(() =>
  normalized.value.find((o) => o.value === props.modelValue)
);
const currentLabel = computed(() =>
  current.value ? current.value.label : String(props.modelValue ?? "")
);
const isPlaceholder = computed(() => !current.value && (props.modelValue === "" || props.modelValue == null));

function toggle() {
  if (props.disabled) return;
  open.value ? close() : show();
}

async function show() {
  open.value = true;
  hi.value = normalized.value.findIndex((o) => o.value === props.modelValue);
  await nextTick();
  const r = root.value?.getBoundingClientRect();
  const p = pop.value;
  if (!r || !p) return;
  const h = p.offsetHeight;
  let top = r.bottom + 5;
  if (top + h > window.innerHeight - 8 && r.top - h - 5 > 8) top = r.top - h - 5; // 下方放不下则上翻
  popStyle.value = { left: r.left + "px", top: top + "px", minWidth: r.width + "px" };
  p.style.maxHeight = Math.min(280, window.innerHeight - top - 8) + "px";
  document.addEventListener("mousedown", onOutside, true);
  document.addEventListener("keydown", onKey, true);
  window.addEventListener("scroll", onScroll, true); // 弹层 fixed 定位,外部滚动即失位,直接关
}

function close() {
  if (!open.value) return;
  open.value = false;
  document.removeEventListener("mousedown", onOutside, true);
  document.removeEventListener("keydown", onKey, true);
  window.removeEventListener("scroll", onScroll, true);
}

// 捕获阶段能收到弹层自身列表的滚动事件,那是选选项的正常操作,不能关
function onScroll(e) {
  if (root.value?.contains(e.target) || pop.value?.contains(e.target)) return;
  close();
}

function onOutside(e) {
  if (!root.value?.contains(e.target) && !pop.value?.contains(e.target)) close();
}

function onKey(e) {
  if (e.key === "Escape") { e.stopPropagation(); close(); return; }
  if (e.key === "ArrowDown" || e.key === "ArrowUp") {
    e.preventDefault();
    const n = normalized.value.length;
    if (!n) return;
    hi.value = ((hi.value < 0 ? 0 : hi.value) + (e.key === "ArrowDown" ? 1 : n - 1)) % n;
    nextTick(() => pop.value?.children[hi.value]?.scrollIntoView({ block: "nearest" }));
  } else if (e.key === "Enter") {
    e.preventDefault();
    if (hi.value >= 0 && hi.value < normalized.value.length) pick(normalized.value[hi.value]);
  }
}

function pick(opt) {
  close();
  if (opt.value === props.modelValue) return;
  emit("update:modelValue", opt.value);
  emit("change", opt.value);
}

onBeforeUnmount(close);
</script>

<template>
  <div class="zselect" ref="root">
    <button type="button" class="zselect-trigger" :class="{ open, disabled }" :disabled="disabled"
            @click="toggle" @keydown.enter.prevent="toggle">
      <span class="zselect-label" :class="{ placeholder: isPlaceholder }">{{ isPlaceholder ? placeholder : currentLabel }}</span>
      <svg class="zselect-chevron" :class="{ open }" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"><path d="M6 9l6 6 6-6"/></svg>
    </button>
    <Teleport to="body">
      <div v-if="open" class="zselect-pop" ref="pop" :style="popStyle">
        <div v-for="(opt, i) in normalized" :key="String(opt.value)" class="zselect-item"
             :class="{ selected: opt.value === modelValue, hi: i === hi }" @click="pick(opt)">
          <span>{{ opt.label }}</span>
          <svg v-if="opt.value === modelValue" class="zselect-check" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"><path d="M5 13l4 4L19 7"/></svg>
        </div>
      </div>
    </Teleport>
  </div>
</template>
