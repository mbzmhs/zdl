<script setup>
// 单张任务卡片:DOM 与展示口径逐行对齐旧 buildCard/updateCard。
// props.t 每个快照整体替换(服务端全量),computed 全量重算即等价旧逐帧刷新。
import { computed } from "vue";
import { store, handleAction, openTaskFile, openOverlay } from "../store.js";
import { fmtBytes, fmtSpeed, fmtETA, fmtTime, extOf, iconClass, iconLabel, magnetFallbackName, STATUS_TEXT } from "../lib/fmt.js";
import { ms, SVGS } from "../lib/icons.js";

const props = defineProps({ t: { type: Object, required: true } });

const displayName = computed(() => props.t.filename || magnetFallbackName(props.t));
const canOpen = computed(() => props.t.status === "completed" || props.t.status === "seeding");
// 单文件视频种子(running/seeding):文件名点击直达边下边播,优先于打开文件
const canStream = computed(() => props.t.streamable && ["running", "seeding"].includes(props.t.status));
// 多文件 BT 种子:标题点击进详情文件页(文件树里可挑文件/播放)
const canFiles = computed(() => props.t.kind === "bt" && (props.t.fileCount || 0) > 1);
const nameClick = () => {
  if (canStream.value) {
    store.player = { taskId: props.t.id, file: "" }; // file 空 = 单文件种子由引擎解析
    openOverlay("playerOverlay");
    return;
  }
  if (canFiles.value) {
    store.pendingTab = "files";
    store.actions.openDrawer && store.actions.openDrawer(props.t.id);
    return;
  }
  if (canOpen.value) openTaskFile(props.t.id);
};
// 文件名 title:完成后提示完整落盘路径;分隔符沿用旧版 "\\"(Mac 批次再按平台取 sep)
const nameTitle = computed(() => canStream.value
  ? "点击边下边播"
  : canFiles.value
    ? "点击查看文件列表"
    : canOpen.value
      ? props.t.dir + "\\" + props.t.filename
      : displayName.value);
const iconText = computed(() => iconLabel(props.t));
const iconCls = computed(() => iconClass(props.t));
const statusText = computed(() => STATUS_TEXT[props.t.status] || props.t.status);

/* 动作按钮(对齐旧 actionsFor):detail 固定首位;openDir 在文件未落定的状态由
   后端退化为开目录;不同状态不同组合 */
const actions = computed(() => {
  const t = props.t, id = t.id;
  const btn = (act, title, html) => ({ act, title, svg: html });
  const detail = btn("detail", "详细属性", SVGS.info);
  const openDir = btn("open", "打开目录", ms("folder_open"));
  switch (t.status) {
    case "running": return [detail, openDir, btn("pause", "暂停", ms("pause")), btn("cancel", "取消", ms("close"))];
    case "paused": return [detail, openDir, btn("resume", "继续", ms("play_arrow")), btn("cancel", "取消", ms("close"))];
    case "waiting": return [detail, openDir, btn("pause", "暂停", ms("pause")), btn("cancel", "取消", ms("close"))];
    case "choosing": return [detail, btn("choosefiles", "创建下载任务(选择文件)", ms("check_circle")),
      btn("resume", "全部下载", ms("play_arrow")), btn("cancel", "取消", ms("close"))];
    case "error": return [detail, openDir, btn("retry", "重试", ms("refresh")), btn("cancel", "取消", ms("close"))];
    case "seeding": return [detail, btn("open", "打开所在文件夹", ms("folder_open")),
      btn("stopseed", "停止做种", ms("stop")), btn("trash", "删除记录", ms("delete"))];
    case "completed": {
      const arr = [detail, btn("open", "打开所在文件夹", ms("folder_open"))];
      if (t.kind === "bt") arr.push(btn("reseed", "重新做种", ms("upload")));
      arr.push(btn("trash", "删除记录", ms("delete")));
      return arr;
    }
    case "canceled": return [detail, btn("resume", "重新下载", ms("refresh")), btn("trash", "删除记录", ms("delete"))];
  }
  return [detail];
});

/* 数值口径(对齐旧 updateCard):BT 用已落盘去重字节 t.arrived——块粒度平滑推进,
   重下/陈旧交付不重复计数,永不超 100%;HTTP/HLS 无此字段,回退累计接收(BT 的
   downloaded 是流量口径,续传后可能超 size,回退时夹取) */
const isHLS = computed(() => props.t.kind === "hls");
const segDone = computed(() => props.t.segDone || 0);
const segTotal = computed(() => props.t.segTotal || 0);
const segPct = computed(() => isHLS.value && segTotal.value > 0 ? (segDone.value / segTotal.value * 100) : 0);
const progBytes = computed(() => {
  const t = props.t;
  return t.arrived > 0 ? t.arrived
    : (t.kind === "bt" && t.size > 0 ? Math.min(t.downloaded, t.size) : t.downloaded);
});
const bytePct = computed(() => props.t.size > 0 ? Math.min(100, progBytes.value / props.t.size * 100) : 0);
const pctVal = computed(() => isHLS.value ? segPct.value : bytePct.value);
const hasPct = computed(() => (isHLS.value && segTotal.value > 0) || props.t.size > 0);

const pctText = computed(() => hasPct.value ? pctVal.value.toFixed(1) + "%" : "—");
const sizeText = computed(() => {
  const t = props.t;
  return isHLS.value && segTotal.value > 0
    ? `${segDone.value}/${segTotal.value} 分片` + (t.downloaded > 0 ? " · " + fmtBytes(t.downloaded) : "") + (t.encrypted ? " · 已加密" : "")
    : `${fmtBytes(progBytes.value)}${t.size > 0 ? " / " + fmtBytes(t.size) : ""}`;
});
const speedText = computed(() => {
  const t = props.t;
  if (t.status === "seeding") return "↑ " + fmtSpeed(t.speed);
  return t.status === "running" ? fmtSpeed(t.speed) : "";
});
const etaText = computed(() => {
  const t = props.t;
  if (t.status === "seeding") {
    const ratio = t.downloaded > 0 ? ((t.uploaded || 0) / t.downloaded).toFixed(2) : "0.00";
    return `分享率 ${ratio} · 已上传 ${fmtBytes(t.uploaded || 0)}`;
  }
  return t.status === "running" && !isHLS.value && t.size > 0
    ? fmtETA(t.speed, t.size - progBytes.value) : "";
});
const rightText = computed(() => {
  const t = props.t;
  return (t.status === "completed" || t.status === "seeding") && t.completedAt && !t.completedAt.startsWith("0001")
    ? "完成于 " + fmtTime(t.completedAt) : "";
});

/* 进度条:completed/seeding 满格 done;有百分比按数值(失败缀 err);运行中无
   百分比用 indeterminate 动画(此时不动 width,与旧逻辑一致);其余归零 */
const fillCls = computed(() => {
  const t = props.t;
  if (t.status === "completed" || t.status === "seeding") return "progress-fill done";
  if (hasPct.value) return "progress-fill" + (t.status === "error" ? " err" : "");
  if (t.status === "running") return "progress-fill indeterminate";
  return "progress-fill";
});
const fillStyle = computed(() => {
  const t = props.t;
  if (t.status === "completed" || t.status === "seeding") return { width: "100%" };
  if (hasPct.value) return { width: Math.min(100, pctVal.value).toFixed(2) + "%" };
  if (t.status === "running") return {}; // indeterminate:宽度交由动画
  return { width: "0%" };
});

/* 分段热度条(多线程下载中显示):压到最多 48 根,每根 = 该段区间完成占比,
   CSS 用 --p 上色;非运行态或单线程隐藏 */
const segBars = computed(() => {
  const t = props.t;
  if (t.status !== "running" || !t.segments || t.segments.length <= 1) return null;
  const segs = t.segments;
  const maxBars = 48;
  const step = Math.ceil(segs.length / maxBars);
  const bars = [];
  for (let i = 0; i < segs.length; i += step) {
    let done = 0, len = 0;
    for (let j = i; j < Math.min(i + step, segs.length); j++) {
      done += segs[j].done; len += segs[j].end - segs[j].start + 1;
    }
    bars.push(len > 0 ? done / len : 1);
  }
  return bars;
});
</script>

<template>
  <div class="task-card" :class="{ 'is-canceled': t.status === 'canceled' }">
    <div class="task-head">
      <div class="file-icon" :class="iconCls">{{ iconText }}</div>
      <div class="task-title">
        <div class="name" :class="{ clickable: canOpen || canStream || canFiles, streamable: canStream }" :title="nameTitle" @click="nameClick">{{ displayName }}<span v-if="canStream" class="name-play" v-html="ms('play_circle')"></span></div>
        <div class="url" :title="t.url">{{ t.url }}</div>
      </div>
      <span class="status-chip" :class="t.status">{{ statusText }}</span>
      <div class="task-actions">
        <button v-for="a in actions" :key="a.act" class="icon-btn" :data-act="a.act" :data-id="t.id" :title="a.title" v-html="a.svg" @click="handleAction(a.act, t.id)"></button>
      </div>
    </div>
    <div class="task-mid">
      <span class="pct">{{ pctText }}</span><span class="size">{{ sizeText }}</span>
      <span class="speed">{{ speedText }}</span><span class="eta">{{ etaText }}</span>
      <span class="error-text" v-show="t.status === 'error'" :title="t.error">{{ t.error || "未知错误" }}</span>
      <span class="right">{{ rightText }}</span>
    </div>
    <div class="progress-track"><div class="progress-fill" :class="fillCls" :style="fillStyle"></div></div>
    <div class="seg-strip" v-show="segBars" :data-n="segBars ? segBars.length : undefined">
      <div v-for="(p, i) in segBars" :key="i" class="seg" :class="{ full: p >= 1 }" :style="{ '--p': p }"></div>
    </div>
  </div>
</template>
