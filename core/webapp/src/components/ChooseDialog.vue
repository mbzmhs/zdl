<script setup>
// BT 文件选择窗口(磁力就绪自动弹出 / 详情改选):文件树勾选、已选统计、
// 全部下载/开始下载。DOM 对齐旧 #chooseOverlay。
import { ref, onMounted, onBeforeUnmount } from "vue";
import { api } from "../lib/api.js";
import { store, openOverlay, closeOverlay, toast, showErr, refreshTasks } from "../store.js";
import { fmtBytes } from "../lib/fmt.js";
import { isPaddingFile } from "../lib/filetree.js";
import FileTree from "./FileTree.vue";
import Overlay from "./Overlay.vue";

const info = ref(""), stat = ref("");
const chFiles = ref([]);
const tree = ref(null);
let chTaskId = null;

async function openChooseDialog(id) {
  let d;
  try { d = await api("GET", `/api/tasks/${id}/detail`); }
  catch (e) { showErr(e); return; }
  const all = d.files || [];
  const files = all.filter(f => !isPaddingFile(f));
  if (!files.length) {
    toast("元数据尚未就绪,稍后再试", "err");
    return;
  }
  chTaskId = id;
  chFiles.value = files;
  const total = files.reduce((s, f) => s + (f.size || 0), 0);
  info.value = `${d.filename || "(未命名)"} · ${files.length} 个文件 · 共 ${fmtBytes(total)}`;
  openOverlay("chooseOverlay");
  updateStat();
}

function updateStat() {
  if (!tree.value) return;
  const st = tree.value.selStat(chFiles.value);
  stat.value = `已选 ${st.n}/${st.total} 个文件 · ${fmtBytes(st.bytes)}` +
    (st.all ? "(全部)" : "");
}

async function submit(body) {
  try {
    await api("POST", `/api/tasks/${chTaskId}/files`, body);
  } catch (e) { showErr(e); return; }
  closeOverlay("chooseOverlay");
  const t = store.tasks.find(x => x.id === chTaskId);
  toast(t && t.status === "paused" ? "文件选择已保存,点「继续」开始下载" : "已开始下载所选文件", "ok");
  refreshTasks();
}
function submitSel() {
  const sel = tree.value ? tree.value.selectedPaths() : null;
  submit(sel === null ? { all: true } : { files: sel });
}

onMounted(() => { store.actions.openChooseDialog = openChooseDialog; });
onBeforeUnmount(() => { if (store.actions.openChooseDialog === openChooseDialog) delete store.actions.openChooseDialog; });
</script>

<template>
  <Overlay id="chooseOverlay">
    <div class="dialog choose-dialog">
      <h2><span class="accent-bar"></span>选择下载文件</h2>
      <div class="choose-sub">{{ info }}</div>
      <div class="ft-scroll">
        <FileTree ref="tree" :files="chFiles" mode="sel" :init-id="'choose:' + chTaskId" @change="updateStat" />
      </div>
      <div class="choose-stat">{{ stat }}</div>
      <div class="dialog-actions">
        <button class="btn btn-ghost" @click="closeOverlay('chooseOverlay')">稍后选择</button>
        <button class="btn btn-ghost" id="chAll" @click="submit({ all: true })">全部下载</button>
        <button class="btn btn-primary" id="chOk" @click="submitSel">开始下载</button>
      </div>
    </div>
  </Overlay>
</template>
