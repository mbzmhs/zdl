<script setup>
// 封禁列表(全局 BT 封禁记录):查看 / 解除单条 / 清空。
// 打开与解除后重拉列表;计数同步给设置页按钮。DOM 对齐旧 #bansOverlay。
import { ref, onMounted } from "vue";
import { api } from "../lib/api.js";
import { closeOverlay, toast, confirmBox, showErr } from "../store.js";
import { esc } from "../lib/fmt.js";
import Overlay from "./Overlay.vue";

const emit = defineEmits(["count"]);
const bans = ref([]);

async function render() {
  let list = [];
  try { list = (await api("GET", "/api/bt/bans")).bans || []; } catch {}
  bans.value = list;
  emit("count", list.length);
}
async function del(ip) {
  try {
    await api("DELETE", "/api/bt/bans/" + encodeURIComponent(ip));
    toast("已解除 " + ip);
  } catch {}
  render();
}
function clearAll() {
  confirmBox("将解除全部封禁记录,被封禁的 IP 立即恢复可连。确认清空?", {
    okLabel: "清空",
    onOk: async () => {
      try {
        const r = await api("DELETE", "/api/bt/bans");
        toast(`已清空 ${r.removed || 0} 条封禁记录`);
      } catch {}
      render();
    },
  });
}
onMounted(render);
defineExpose({ render });
</script>

<template>
  <Overlay id="bansOverlay">
    <div class="dialog" style="width:min(640px,calc(100vw - 48px))">
      <h2><span class="accent-bar"></span>封禁列表<span class="dim" style="font-size:13px;font-weight:400">{{ bans.length ? `(${bans.length})` : "" }}</span></h2>
      <p style="color:var(--text-dim);font-size:12.5px;margin:-8px 0 14px">全部 BT 任务共享;封禁中的 IP 无法向本程序发起任何连接。解除后该 IP 立即恢复可连。</p>
      <div style="max-height:min(52vh,460px);overflow:auto">
        <div class="bans-table" id="bansTable">
          <div v-if="!bans.length" style="text-align:center;color:var(--text-faint);padding:34px 0;font-size:12.5px">暂无封禁记录</div>
          <template v-else>
            <div class="bans-row bans-head"><span>IP</span><span>客户端</span><span>封禁时间</span><span>原因</span><span></span></div>
            <div v-for="b in bans" :key="b.ip" class="bans-row">
              <span class="bans-ip" :title="esc(b.ip)">{{ b.ip }}<span v-if="b.count > 1" class="dim" :title="`累计封禁 ${b.count} 次`"> ×{{ b.count }}</span></span>
              <span class="bans-reason" :title="esc(b.client || '')">{{ b.client || "—" }}</span>
              <span class="bans-time">{{ new Date(b.time).toLocaleString("zh-CN", { hour12: false }) }}</span>
              <span class="bans-reason" :title="esc(b.reason || '')">{{ b.reason || "—" }}</span>
              <button type="button" class="icon-btn bans-del" title="解除封禁" @click="del(b.ip)">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><path d="M18 6L6 18M6 6l12 12"/></svg>
              </button>
            </div>
          </template>
        </div>
      </div>
      <div class="dialog-actions">
        <button class="btn btn-ghost" id="bansClear" style="color:var(--err)" @click="clearAll">清空全部</button>
        <span style="flex:1"></span>
        <button class="btn btn-ghost" @click="closeOverlay('bansOverlay')">关闭</button>
      </div>
    </div>
  </Overlay>
</template>
