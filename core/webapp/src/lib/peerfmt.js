// 详情明细表的格式化与排序(逐行对齐旧 app.js)

export function fmtDur(sec) {
  if (!sec || sec < 0) return "—";
  const s = Math.floor(sec);
  if (s < 60) return `0:${String(s).padStart(2, "0")}`;
  if (s < 3600) return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
  return `${Math.floor(s / 3600)}:${String(Math.floor(s % 3600 / 60)).padStart(2, "0")}:${String(s % 60).padStart(2, "0")}`;
}

export const SRC_TEXT = { tracker: "Tracker", dht: "DHT", pex: "PEX", manual: "手动", incoming: "入站", history: "历史" };

function cmpVal(a, b) {
  if (typeof a === "number" && typeof b === "number") return a - b;
  const sa = String(a ?? ""), sb = String(b ?? "");
  return sa < sb ? -1 : sa > sb ? 1 : 0;
}

// IPv4 按点分段数值比较(避免 2.x 排在 10.x 后面),其余走字符串比较
export function cmpIp(a, b) {
  const pa = String(a).split("."), pb = String(b).split(".");
  if (pa.length === 4 && pb.length === 4) {
    for (let i = 0; i < 4; i++) {
      const d = (+pa[i] || 0) - (+pb[i] || 0);
      if (d) return d;
    }
  }
  return cmpVal(a, b);
}

// rows: [{v: 排序键值表, ...}],primary 为主键,tie 为并列时的次序键
export function sortRows(rows, sort, tie) {
  rows.sort((x, y) => {
    let d = sort.key === "ip" ? cmpIp(x.v.ip, y.v.ip) : cmpVal(x.v[sort.key], y.v[sort.key]);
    if (d) return d * sort.dir;
    for (const k of tie) {
      d = k === "ip" ? cmpIp(x.v.ip, y.v.ip) : cmpVal(x.v[k], y.v[k]);
      if (d) return d;
    }
    return 0;
  });
  return rows;
}

// 选中文本保护:表内有未收起的选区时跳过该表重渲染,避免每秒全量重建
// 把用户正在复制的内容选区清掉
export function selectionIn(el) {
  const sel = window.getSelection();
  if (!sel || sel.isCollapsed || !sel.rangeCount) return false;
  return el.contains(sel.getRangeAt(0).commonAncestorContainer);
}
