<script setup>
// 单个订阅行 + 其条目区(RSS 面板的树节点,文件夹行与根分组各自渲染一次本组件)
import { rss, rssToggleFeed } from "../rss.js";
import { ms } from "../lib/icons.js";
import { fmtBytes } from "../lib/fmt.js";
import { fmtAgo, fmtItemDate, rssLinkName, rssItemRes } from "../lib/rssfmt.js";

const props = defineProps({ f: { type: Object, required: true } });
const CARET_PATH = "M6 9l6 6 6-6";
const open = () => rss.openFeeds.has(props.f.id);

function feedMeta(f) {
  if ((rss.tree.updating || []).includes(f.id)) return { upd: true, text: "更新中…" };
  if (f.lastError) return { bad: true, text: "更新失败", tip: f.lastError };
  return { text: `${fmtAgo(f.lastFetch)} · ${f.itemCount || 0} 条` };
}
const isMagnet = url => /^magnet:/i.test(url);
function linkRows(it) {
  const rows = [...(it.links || [])];
  // 条目页面本身也是一条候选链接(标"页面",排在附件之后)
  if (it.link && !rows.some(l => l.url === it.link)) rows.push({ url: it.link, page: true });
  return rows;
}
function linkInfo(l) {
  return [l.length ? fmtBytes(l.length) : "", l.type || ""].filter(Boolean).join(" · ");
}
function dropCls(id) {
  if (rss.dropMark.key !== id) return {};
  return rss.dropMark.pos === "before" ? { "drop-before": true } : { "drop-after": true };
}
function onSelLink(e) {
  const cb = e.target;
  if (cb.checked) rss.sel.set(cb.dataset.url, { url: cb.dataset.url, name: cb.dataset.name });
  else rss.sel.delete(cb.dataset.url);
}
function onSelItem(e, it) {
  // 折叠条目的"全选资源链接":批量勾选/取消该条目全部资源链接(不含页面/磁力)
  for (const l of rssItemRes(it)) {
    if (e.target.checked) rss.sel.set(l.url, { url: l.url, name: rssLinkName(l) });
    else rss.sel.delete(l.url);
  }
}
defineExpose({});
</script>

<template>
  <div class="rss-feed" :class="{ open: open(), ...dropCls(f.id) }"
    draggable="true" data-drag="feed" :data-feed="f.id" data-drop="feed">
    <span class="rss-caret" data-rss="toggle-feed" :data-feed="f.id">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"><path :d="CARET_PATH"/></svg>
    </span>
    <span class="ms" v-html="ms('article')"></span>
    <span class="n" :title="f.url">{{ f.title || f.siteTitle || f.url }}</span>
    <span v-if="(f.unread || 0) > 0" class="rss-badge">{{ f.unread }}</span>
    <span class="meta">
      <span v-if="feedMeta(f).upd" class="upd">更新中…</span>
      <span v-else-if="feedMeta(f).bad" class="bad" :title="feedMeta(f).tip">更新失败</span>
      <span v-else>{{ feedMeta(f).text }}</span>
    </span>
    <span class="row-acts">
      <button class="icon-btn" data-rss="refreshfeed" :data-feed="f.id" title="刷新该订阅" v-html="ms('refresh')"></button>
      <button class="icon-btn" data-rss="readfeed" :data-feed="f.id" title="全部标为已读" v-html="ms('check')"></button>
      <button class="icon-btn" data-rss="editfeed" :data-feed="f.id" title="编辑(名称/文件夹)" v-html="ms('movie_edit')"></button>
      <button class="icon-btn" data-rss="delfeed" :data-feed="f.id" title="删除订阅" v-html="ms('delete')"></button>
    </span>
  </div>
  <div class="rss-items" v-show="open()">
    <div v-if="!rss.items.get(f.id)" class="rss-hint">加载中…</div>
    <div v-else-if="!rss.items.get(f.id).length" class="rss-hint">暂无条目,可点该订阅的刷新按钮重试</div>
    <template v-else>
      <div v-for="it in rss.items.get(f.id)" :key="it.id" class="rss-item"
        :class="{ unread: !it.read, open: rss.openItems.has(it.id) }" :data-item="it.id">
        <span class="rss-caret" data-rss="toggle-item" :data-item="it.id"
          :title="rss.openItems.has(it.id) ? '收起资源链接' : '展开资源链接'">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"><path :d="CARET_PATH"/></svg>
        </span>
        <label v-if="!rss.openItems.has(it.id) && rssItemRes(it).length" class="rss-item-cb"
          title="勾选本条目的全部资源链接(不含页面)">
          <input type="checkbox" data-rss="sel-item" :data-item="it.id"
            :checked="rssItemRes(it).every(l => rss.sel.has(l.url))"
            @change="onSelItem($event, it)" @click.stop>
        </label>
        <span class="udot" :title="it.read ? '已读' : '未读'"></span>
        <div class="rss-item-main">
          <div class="rss-item-title">{{ it.title || "(无标题)" }}</div>
          <div class="rss-item-sub">
            <span v-if="it.date" class="date">{{ fmtItemDate(it.date) }}</span>
            <span v-if="it.summary" class="sum" :title="it.summary">{{ it.summary }}</span>
          </div>
        </div>
        <span class="row-acts">
          <button class="icon-btn" data-rss="toggle-read" :data-item="it.id" :data-read="it.read ? '1' : '0'"
            :title="it.read ? '标为未读' : '标为已读'" v-html="ms(it.read ? 'refresh' : 'check')"></button>
        </span>
        <div class="rss-links" v-if="rss.openItems.has(it.id)">
          <template v-if="linkRows(it).length">
            <div v-for="l in linkRows(it)" :key="l.url" class="rss-link" :class="{ sel: rss.sel.has(l.url) }">
              <input type="checkbox" data-rss="sel-link" :data-url="l.url" :data-name="rssLinkName(l)"
                :checked="rss.sel.has(l.url)" @change="onSelLink"
                :disabled="isMagnet(l.url)" :title="isMagnet(l.url) ? '磁力链接请单独创建' : undefined">
              <div class="rss-link-main">
                <div class="t" :title="l.url">{{ rssLinkName(l) }}<span v-if="l.page" class="tag"> 页面</span><span v-if="isMagnet(l.url)" class="tag"> 磁力</span></div>
                <div v-if="linkInfo(l)" class="s">{{ linkInfo(l) }}</div>
              </div>
              <button v-if="l.page" class="btn btn-ghost btn-sm" data-rss="prev-one" :data-url="l.url" :data-name="rssLinkName(l)">预览</button>
              <button v-else class="btn btn-ghost btn-sm" data-rss="dl-one" :data-url="l.url" :data-name="rssLinkName(l)">下载</button>
            </div>
          </template>
          <div v-else class="rss-hint">该条目没有可直接下载的链接</div>
        </div>
      </div>
    </template>
  </div>
</template>
