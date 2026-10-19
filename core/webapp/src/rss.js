// RSS 状态与操作(旧 app.js RSS 段的状态模型平移到响应式)。
// 树:文件夹(可选)→ 订阅 → 条目 → 资源链接。侧栏徽章显示未读合计,
// 条目链接可直接预填新建任务窗(单条/批量勾选)。
import { reactive } from "vue";
import { api } from "./lib/api.js";
import { store, toast, showErr, confirmBox, openOverlay } from "./store.js";
import { rssLinkName, rssItemRes } from "./lib/rssfmt.js";

export const rss = reactive({
  tree: { folders: [], feeds: [], unread: 0, updating: [], busy: false },
  items: new Map(),       // feedId -> Item[]
  itemsStamp: new Map(),  // feedId -> 上次条目缓存对应的 lastFetch
  openFeeds: new Set(),   // 展开的订阅
  collapsedFolders: new Set(), // 折叠的文件夹(展开态仅本次会话有效)
  foldersSeeded: false,   // 启动首载时把所有文件夹置为折叠
  openItems: new Set(),   // 展开资源链接的条目
  sel: new Map(),         // 已勾选的下载链接 url -> {url, name}
  pollTimer: 0,
  dropMark: { key: "", pos: "" }, // 拖拽落点标记(文件夹行="F:"+id;订阅行=feed id)
});

const rssFeedById = id => (rss.tree.feeds || []).find(f => f.id === id);
const rssFeedsOf = fid => (rss.tree.feeds || []).filter(f => (f.folderId || "") === fid)
  .sort((a, b) => (a.order || 0) - (b.order || 0));

export async function rssLoad() {
  let t;
  try { t = await api("GET", "/api/rss"); } catch (_) { return; }
  rss.tree = t;
  // 侧栏徽章(与旧 rssBadge(t.unread) 等价:走 stats 供 SideBar 读取)
  store.stats.rssUnread = t.unread || 0;
  // 软件刚启动时文件夹(含未分组)一律折叠,避免进入 RSS 页就是一大片展开的树;
  // 首载之后完全跟随用户手动展开/折叠
  if (!rss.foldersSeeded) {
    rss.foldersSeeded = true;
    for (const fo of t.folders || []) rss.collapsedFolders.add("F:" + fo.id);
    rss.collapsedFolders.add("F:");
  }
  // 已删除的订阅:清理展开态与条目缓存
  const ids = new Set((t.feeds || []).map(f => f.id));
  for (const id of [...rss.openFeeds]) if (!ids.has(id)) rss.openFeeds.delete(id);
  for (const id of [...rss.items.keys()]) if (!ids.has(id)) rss.items.delete(id);
  if (store.currentView !== "rss") return;
  // 已展开的订阅若刚被服务端抓取过(含自动更新),同步刷新其条目
  for (const id of rss.openFeeds) {
    const f = rssFeedById(id);
    if (f && rss.items.has(id) && rss.itemsStamp.get(id) !== f.lastFetch) {
      await rssLoadItems(id, { rerender: false });
    }
  }
}

export async function rssLoadItems(id) {
  try {
    const r = await api("GET", "/api/rss/items?feed=" + encodeURIComponent(id));
    rss.items.set(id, r.items || []);
  } catch (e) {
    rss.items.set(id, []);
    showErr(e);
  }
  const f = rssFeedById(id);
  rss.itemsStamp.set(id, f ? f.lastFetch : "");
}

function rssFindItem(id) {
  for (const [, list] of rss.items) {
    const it = list.find(x => x.id === id);
    if (it) return it;
  }
  return null;
}

/* ---- 展开/折叠 ---- */
export function rssToggleFolder(id) {
  const key = "F:" + id;
  if (rss.collapsedFolders.has(key)) rss.collapsedFolders.delete(key);
  else rss.collapsedFolders.add(key);
}

export async function rssToggleFeed(id) {
  if (rss.openFeeds.has(id)) {
    rss.openFeeds.delete(id);
    return;
  }
  rss.openFeeds.add(id);
  await rssLoadItems(id);
}

// 展开条目即视为已读(与是否下载无关);收起不改变已读状态
export async function rssToggleItem(id) {
  if (rss.openItems.has(id)) {
    rss.openItems.delete(id);
    return;
  }
  rss.openItems.add(id);
  const it = rssFindItem(id);
  const wasUnread = !!it && !it.read;
  if (wasUnread) it.read = true; // 先本地生效,未读计数随后由 rssLoad 校准
  if (wasUnread) {
    try { await api("POST", `/api/rss/items/${id}/read`, { read: true }); } catch (_) {}
    rssLoad();
  }
}

/* ---- 勾选 ---- */
export function selLink(cb) {
  if (cb.checked) rss.sel.set(cb.dataset.url, { url: cb.dataset.url, name: cb.dataset.name });
  else rss.sel.delete(cb.dataset.url);
}
export function selItem(itemCb) {
  // 折叠条目的"全选资源链接"外层复选框:批量勾选/取消该条目全部资源链接
  // (不含页面/磁力)。展开后逐行复选框依据同一 rss.sel 渲染,勾选态自然保留。
  const it = rssFindItem(itemCb.dataset.item);
  if (!it) return;
  for (const l of rssItemRes(it)) {
    if (itemCb.checked) rss.sel.set(l.url, { url: l.url, name: rssLinkName(l) });
    else rss.sel.delete(l.url);
  }
}
export function selClear() { rss.sel.clear(); }

// 从资源链接创建任务:复用新建任务窗(文件名留空、按各文件默认名;每行一个
// 链接),磁力单独走 BT 窗口
export function rssDownload(links) {
  const mags = links.filter(l => /^magnet:/i.test(l.url));
  const urls = links.filter(l => !/^magnet:/i.test(l.url)).map(l => l.url);
  if (!urls.length) {
    if (mags.length > 1) toast("磁力链接一次只能创建一个 BT 任务,已填入第一条");
    store.actions.openBTDialog && store.actions.openBTDialog(mags[0].url);
  } else {
    if (mags.length) toast(`已跳过 ${mags.length} 个磁力链接(请单独创建)`, "");
    store.actions.openNewTask && store.actions.openNewTask(urls, "", true);
  }
  rss.sel.clear();
}

/* ---- 订阅 / 文件夹编辑弹窗 ---- */
export const rssFeedDlg = reactive({ editId: null, edit: null, folderId: "" });
export function openRssFeedDialog({ folderId = "", edit = null } = {}) {
  rssFeedDlg.editId = edit ? edit.id : null;
  rssFeedDlg.edit = edit || null;
  rssFeedDlg.folderId = folderId;
  openOverlay("rssFeedOverlay");
  if (!edit) setTimeout(() => document.getElementById("rssFeedUrl")?.focus(), 0);
}
export const rssFolderDlg = reactive({ editId: null });
export function openRssFolderDialog(id = "") {
  rssFolderDlg.editId = id || null;
  openOverlay("rssFolderOverlay");
  setTimeout(() => document.getElementById("rssFolderName")?.focus(), 0);
}

/* ---- 行内操作 ---- */
export async function rssAction(act, el) {
  const feedId = el.dataset.feed || "";
  const folderId = el.dataset.folder || "";
  const itemId = el.dataset.item || "";
  try {
    switch (act) {
      case "addfeed": openRssFeedDialog({ folderId: el.dataset.folder }); break;
      case "newfolder": openRssFolderDialog(); break;
      case "renamefolder": openRssFolderDialog(folderId); break;
      case "delfolder": {
        const fo = (rss.tree.folders || []).find(f => f.id === folderId);
        confirmBox(`删除文件夹「${fo ? fo.name : ""}」?其中订阅会移到「未分组」,不会被删除。`, {
          okLabel: "删除",
          onOk: async () => {
            try { await api("DELETE", "/api/rss/folders/" + folderId); } catch (e) { showErr(e); return; }
            toast("文件夹已删除");
            rssLoad();
          },
        });
        break;
      }
      case "readall":
        await api("POST", "/api/rss/read");
        for (const id of rss.openFeeds) rss.items.delete(id);
        await rssLoad();
        toast("已全部标为已读");
        break;
      case "refreshall":
        await api("POST", "/api/rss/refresh");
        for (const id of rss.openFeeds) rss.items.delete(id);
        toast("已开始更新全部订阅");
        await rssLoad();
        break;
      case "refreshfeed":
        rss.items.delete(feedId);
        await api("POST", `/api/rss/feeds/${feedId}/refresh`);
        toast("已开始更新");
        await rssLoad();
        if (rss.openFeeds.has(feedId)) await rssLoadItems(feedId);
        break;
      case "readfeed":
        await api("POST", `/api/rss/feeds/${feedId}/read`, { read: true });
        rss.items.delete(feedId);
        await rssLoad();
        if (rss.openFeeds.has(feedId)) await rssLoadItems(feedId);
        break;
      case "editfeed": {
        const f = rssFeedById(feedId);
        if (f) openRssFeedDialog({ edit: f });
        break;
      }
      case "delfeed": {
        const f = rssFeedById(feedId);
        const nm = f ? (f.title || f.siteTitle || f.url) : "";
        confirmBox(`删除订阅「${nm}」?条目与已读状态一并删除,已创建的任务不受影响。`, {
          okLabel: "删除",
          onOk: async () => {
            try { await api("DELETE", "/api/rss/feeds/" + feedId); } catch (e) { showErr(e); return; }
            rss.openFeeds.delete(feedId);
            rss.items.delete(feedId);
            toast("订阅已删除");
            rssLoad();
          },
        });
        break;
      }
      case "toggle-folder": rssToggleFolder(folderId); break;
      case "toggle-feed": await rssToggleFeed(feedId); break;
      case "toggle-item": rssToggleItem(itemId); break;
      case "toggle-read": {
        const read = el.dataset.read === "1";
        await api("POST", `/api/rss/items/${itemId}/read`, { read: !read });
        const hit = rssFindItem(itemId);
        if (hit) hit.read = !read;
        rssLoad(); // 徽章/未读计数
        break;
      }
      case "dl-one":
        rssDownload([{ url: el.dataset.url, name: el.dataset.name }]);
        break;
      case "prev-one":
        store.actions.openRssPreview && store.actions.openRssPreview(el.dataset.url, el.dataset.name);
        break;
      case "sel-clear":
        rss.sel.clear();
        break;
      case "sel-download":
        if (rss.sel.size) rssDownload([...rss.sel.values()]);
        break;
    }
  } catch (e) { showErr(e); }
}

/* ---- 拖拽:文件夹排序 / 订阅换文件夹与排序 ---- */
export function rssDropPos(e, el) {
  const r = el.getBoundingClientRect();
  return e.clientY < r.top + r.height / 2 ? "before" : "after";
}

async function rssDropFolder(id, targetId, pos) {
  if (!id || id === targetId) return;
  const ids = (rss.tree.folders || []).map(f => f.id);
  const from = ids.indexOf(id);
  if (from < 0 || ids.indexOf(targetId) < 0) return;
  ids.splice(from, 1);
  let at = ids.indexOf(targetId);
  if (pos === "after") at += 1;
  ids.splice(at, 0, id);
  await api("POST", "/api/rss/folders/order", { ids });
}

async function rssDropFeed(id, zone, pos) {
  if (!zone) return;
  let target;
  if (zone.dataset.drop === "folder") {
    target = { folderId: zone.dataset.folder || "", beforeId: "" }; // 放入文件夹末尾
  } else {
    const t = rssFeedById(zone.dataset.feed);
    if (!t || t.id === id) return;
    const list = rssFeedsOf(t.folderId || "").filter(f => f.id !== id);
    const i = list.findIndex(f => f.id === t.id);
    target = {
      folderId: t.folderId || "",
      beforeId: pos === "before" ? t.id : ((list[i + 1] || {}).id || ""),
    };
  }
  // 落点与现状一致(同文件夹、前后邻居不变)就不必请求
  const cur = rssFeedById(id);
  const inFolder = rssFeedsOf(target.folderId);
  const curIdx = inFolder.findIndex(f => f.id === id);
  if (cur && curIdx >= 0) {
    const rest = inFolder.filter(f => f.id !== id);
    const curNextId = (rest[curIdx] || {}).id || "";
    if ((cur.folderId || "") === target.folderId && target.beforeId === curNextId) return;
  }
  await api("POST", `/api/rss/feeds/${id}/move`, target);
}

export function makeRssDnd(paneEl) {
  let dragRow = null;
  const onDragStart = e => {
    const row = e.target.closest("[data-drag]");
    if (!row) return;
    dragRow = row;
    row.classList.add("dragging");
    e.dataTransfer.effectAllowed = "move";
    e.dataTransfer.setData("text/plain", JSON.stringify({
      type: row.dataset.drag,
      id: row.dataset.drag === "folder" ? row.dataset.folder : row.dataset.feed,
    }));
  };
  const onDragEnd = () => {
    if (dragRow) dragRow.classList.remove("dragging");
    dragRow = null;
    rss.dropMark.key = ""; rss.dropMark.pos = "";
  };
  const onDragOver = e => {
    if (!dragRow) return;
    const zone = e.target.closest("[data-drop]");
    rss.dropMark.key = ""; rss.dropMark.pos = "";
    if (!zone) return;
    if (dragRow.dataset.drag === "folder" && zone.dataset.drop !== "folder") return; // 文件夹只在分组间排序
    e.preventDefault();
    e.dataTransfer.dropEffect = "move";
    rss.dropMark.key = zone.dataset.drop === "folder"
      ? "F:" + (zone.dataset.folder || "")
      : zone.dataset.feed || "";
    rss.dropMark.pos = rssDropPos(e, zone);
  };
  const onDrop = async e => {
    if (!dragRow) return;
    const zone = e.target.closest("[data-drop]");
    const row = dragRow;
    dragRow = null;
    row.classList.remove("dragging");
    rss.dropMark.key = ""; rss.dropMark.pos = "";
    if (!zone) return;
    e.preventDefault();
    const pos = rssDropPos(e, zone);
    try {
      if (row.dataset.drag === "folder") await rssDropFolder(row.dataset.folder, zone.dataset.folder, pos);
      else await rssDropFeed(row.dataset.feed, zone, pos);
    } catch (err) { showErr(err); }
    await rssLoad();
  };
  paneEl.addEventListener("dragstart", onDragStart);
  paneEl.addEventListener("dragend", onDragEnd);
  paneEl.addEventListener("dragover", onDragOver);
  paneEl.addEventListener("drop", onDrop);
  return () => {
    paneEl.removeEventListener("dragstart", onDragStart);
    paneEl.removeEventListener("dragend", onDragEnd);
    paneEl.removeEventListener("dragover", onDragOver);
    paneEl.removeEventListener("drop", onDrop);
  };
}

// RSS 视图可见时轮询(抓取/新条目/未读变化都能及时反映)
export function startRssPoll() {
  stopRssPoll();
  rss.pollTimer = setInterval(() => {
    if (store.currentView === "rss" && !document.hidden) rssLoad();
  }, 4000);
}
export function stopRssPoll() {
  if (rss.pollTimer) clearInterval(rss.pollTimer);
  rss.pollTimer = 0;
}
