<script setup>
// 客户端黑名单管理(设置 → BT 下载):通配符模式增删改,保存即落盘热生效。
// 打开时从草稿快照建行;保存去重后 PUT,响应回填按钮计数。DOM 对齐旧 #clientBLOverlay。
import { ref, watch, nextTick } from "vue";
import { api } from "../lib/api.js";
import { ui, closeOverlay, toast, showErr } from "../store.js";
import Overlay from "./Overlay.vue";

const props = defineProps({ draft: { type: Array, default: () => [] } });
const emit = defineEmits(["saved"]);
const rows = ref([]); // 字符串行
const rowsEl = ref(null);

watch(() => ui.flags.clientBLOverlay, v => {
  if (!v) return;
  rows.value = props.draft.length ? [...props.draft] : [""];
  nextTick(() => {
    const inputs = rowsEl.value?.querySelectorAll("input");
    inputs && inputs[0] && inputs[0].focus();
  });
});

function addRow() {
  rows.value.push("");
  nextTick(() => {
    const inputs = rowsEl.value?.querySelectorAll("input");
    inputs && inputs[inputs.length - 1].focus();
  });
}
function rmRow(i) { rows.value.splice(i, 1); }

async function save() {
  const seen = new Set();
  const patterns = rows.value.map(s => s.trim()).filter(Boolean)
    .filter(p => !seen.has(p) && seen.add(p));
  try {
    const r = await api("PUT", "/api/bt/client-blacklist", { patterns });
    const final = r.patterns || patterns;
    emit("saved", final);
    closeOverlay("clientBLOverlay");
    toast(final.length ? `客户端黑名单已保存(${final.length} 条)` : "客户端黑名单已清空");
  } catch (e) { showErr(e); }
}
</script>

<template>
  <Overlay id="clientBLOverlay">
    <div class="dialog" style="width:min(560px,calc(100vw - 48px))">
      <h2><span class="accent-bar"></span>客户端黑名单</h2>
      <div class="hint-text">每行一条模式:星号 * 匹配任意长度、问号 ? 匹配单个字符,大小写不敏感(如 *Xunlei*、*Thunder*)。命中即写入全局封禁名单并断开连接,保存后立即生效</div>
      <div ref="rowsEl" style="display:flex;flex-direction:column;gap:8px;max-height:46vh;overflow:auto">
        <div v-for="(v, i) in rows" :key="i" class="q-row bl-row">
          <input type="text" class="mono" placeholder="*Xunlei*" spellcheck="false" v-model="rows[i]">
          <button type="button" class="btn btn-ghost btn-sm" @click="rmRow(i)">删除</button>
        </div>
      </div>
      <div style="margin-top:10px">
        <button type="button" class="btn btn-ghost btn-sm" id="blAddRow" @click="addRow">+ 添加模式</button>
      </div>
      <div class="dialog-actions">
        <button class="btn btn-ghost" @click="closeOverlay('clientBLOverlay')">取消</button>
        <button class="btn btn-primary" id="blSave" @click="save">保存</button>
      </div>
    </div>
  </Overlay>
</template>
