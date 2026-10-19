<script setup>
// 创建对话框的保存目录输入框 = 可编辑下拉:点 ▾(或聚焦)展开,首行固定为
// 全局默认目录(灰色标识),其下为最近使用记录(最新在上,逐条可 × 删除);
// 与默认相同的记录不重复展示。点选回填并标记 userDir。展开时先取一次最新
// 配置再渲染(多窗口/外部改动即时反映);popover 顶层渲染防对话框裁剪。
import { ref, onMounted, onBeforeUnmount } from "vue";
import { api } from "../lib/api.js";
import { store, deleteRecentDir } from "../store.js";

const props = defineProps({
  placeholder: { type: String, default: "默认下载目录" },
});
const model = defineModel({ type: String, default: "" });
const userDir = defineModel("userDir", { type: Boolean, default: false });

const field = ref(null), input = ref(null), drop = ref(null), panel = ref(null);
const rows = ref([]); // [{path, isDef}]

const isOpen = () => panel.value?.matches(":popover-open");

function render() {
  const def = store.cfg ? store.cfg.downloadDir : "";
  const list = ((store.cfg && store.cfg.recentDirs) || [])
    .filter(p => !def || p.toLowerCase() !== def.toLowerCase());
  rows.value = [];
  if (def) rows.value.push({ path: def, isDef: true });
  for (const p of list) rows.value.push({ path: p, isDef: false });
}

function onDoc(e) {
  if (!panel.value.contains(e.target) && !drop.value.contains(e.target) && e.target !== input.value) hide();
}
function onScroll(e) {
  if (!panel.value.contains(e.target)) hide(); // 面板内滚动不收起;面板外滚动即收起防错位
}
function hide() {
  if (isOpen()) panel.value.hidePopover();
  document.removeEventListener("mousedown", onDoc, true);
  document.removeEventListener("scroll", onScroll, true);
}
function place() {
  const r = field.value.getBoundingClientRect();
  panel.value.style.left = r.left + "px";
  panel.value.style.top = r.bottom + 6 + "px";
  panel.value.style.width = r.width + "px";
}
async function show() {
  try {
    const c = await api("GET", "/api/config");
    if (c && store.cfg) {
      store.cfg.recentDirs = c.recentDirs || [];
      store.cfg.downloadDir = c.downloadDir || store.cfg.downloadDir;
    }
  } catch (_) {}
  render();
  place();
  panel.value.showPopover();
  document.addEventListener("mousedown", onDoc, true);
  document.addEventListener("scroll", onScroll, true);
}
function toggle(e) {
  e.stopPropagation();
  isOpen() ? hide() : show();
}
function pick(p) {
  model.value = p;
  userDir.value = true; // 点选默认/最近路径也是手动指定,创建成功后回写排序
  hide();
}
function onInput() {
  userDir.value = true; // 手动输入 = 手动指定
  hide(); // 手动输入时收起,不挡视线
}
async function del(e, path) {
  e.stopPropagation();
  await deleteRecentDir(path);
  render(); // 删除后原位刷新列表
}
onMounted(() => { if (store.cfg) render(); });
onBeforeUnmount(hide);

defineExpose({ inputEl: input });
</script>

<template>
  <div class="dir-field" ref="field">
    <input type="text" ref="input" v-model="model" :placeholder="placeholder"
      autocomplete="off" spellcheck="false"
      @focus="isOpen() || show()" @input="onInput">
    <button type="button" class="dir-drop" ref="drop" tabindex="-1" title="最近使用的路径" @click="toggle">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="M6 9l6 6 6-6"/></svg>
    </button>
    <div class="dir-recent" ref="panel" popover="manual">
      <div v-for="r in rows" :key="r.path" class="dir-item" :class="{ 'dir-def': r.isDef }">
        <button type="button" class="dir-item-pick" @click="pick(r.path)">
          <span v-if="r.isDef" class="dir-def-tag">默认</span>
          <span>{{ r.path }}</span>
        </button>
        <button v-if="!r.isDef" type="button" class="dir-item-del" title="删除该记录" @click="del($event, r.path)">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><path d="M18 6L6 18M6 6l12 12"/></svg>
        </button>
      </div>
      <div v-if="!rows.length" class="dir-recent-empty">暂无最近使用的路径</div>
    </div>
  </div>
</template>
