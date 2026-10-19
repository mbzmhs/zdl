<script>
// BitComet 式文件树表格(创建对话框/文件选择窗/详情页共用)。
// 用 render 函数递归输出,DOM 与旧 ftNodeHTML 逐节点同构:
// .ft-row 行 + 恒存在的 .ft-children 容器(折叠=display:none)。
// 选择状态集中在 sel Map(key=相对路径);目录复选框由后代推导(全勾=勾,部分=半选)。
import { reactive, computed, watch, h } from "vue";
import { buildFileTree, ftAgg, ftPieceText, fileIcon, isPaddingFile } from "../lib/filetree.js";
import { ms } from "../lib/icons.js";
import { fmtBytes } from "../lib/fmt.js";

const CARET = () => h("svg", { class: "caret", viewBox: "0 0 24 24", fill: "none", stroke: "currentColor", "stroke-width": "2.4", "stroke-linecap": "round" },
  h("path", { d: "M9 6l6 6-6 6" }));

export default {
  name: "FileTree",
  props: {
    files: { type: Array, default: () => [] }, // 已剔除填充文件的展示清单
    mode: { type: String, default: "sel" },    // sel=复选框选择;view=详情只读(editable 时带复选框)
    editable: { type: Boolean, default: false },
    taskStatus: { type: String, default: "" },
    initId: { type: String, default: "" },     // 变化即重置折叠/勾选(旧 initFileTreeState 语义)
  },
  emits: ["change"],
  setup(props, { emit, expose }) {
    const state = reactive({ sel: new Map(), collapsed: new Set() });

    function resetState() {
      const sel = new Map(), collapsed = new Set();
      for (const f of props.files) {
        sel.set(f.path.join("/"), props.mode === "sel" ? true : f.wanted !== false);
      }
      // 大种子默认折叠顶层目录,避免一屏刷出上千行
      if (props.files.length > 60) {
        const tops = new Set();
        for (const f of props.files) tops.add(f.path[0]);
        for (const t of tops) collapsed.add(t);
      }
      state.sel = sel;
      state.collapsed = collapsed;
    }
    watch(() => props.initId, resetState, { immediate: true });
    watch(() => props.files, () => { if (!props.initId) resetState(); });

    const tree = computed(() => buildFileTree(props.files));

    const withCb = () => props.mode === "sel" || props.editable;

    /* ---- 选择操作(供父组件调用,语义同旧 ftSelectedPaths/ftSelStat) ---- */
    function selectedPaths() {
      const keys = [...state.sel.keys()];
      if (!keys.length || keys.every(k => state.sel.get(k))) return null;
      return keys.filter(k => state.sel.get(k));
    }
    function selStat(files) {
      const list = files || props.files;
      const sel = selectedPaths();
      if (sel === null) {
        let bytes = 0;
        for (const f of list) bytes += f.size || 0;
        return { n: list.length, total: list.length, bytes, all: true };
      }
      const set = new Set(sel);
      let n = 0, bytes = 0;
      for (const f of list) {
        if (set.has(f.path.join("/"))) { n++; bytes += f.size || 0; }
      }
      return { n, total: list.length, bytes, all: false };
    }
    function setAll(on) {
      for (const k of [...state.sel.keys()]) state.sel.set(k, on);
      emit("change");
    }
    // 仅重置勾选(折叠状态保留):详情页秒级轮询在非编辑态按服务端 wanted 重建
    // 勾选集,与旧「innerHTML 重建但 fileTreeCollapsed 不变」一致
    function selReset() {
      for (const f of props.files) {
        state.sel.set(f.path.join("/"), props.mode === "sel" ? true : f.wanted !== false);
      }
    }
    expose({ selectedPaths, selStat, setAll, selReset, isPaddingFile });

    const toggleCollapse = key => {
      if (state.collapsed.has(key)) state.collapsed.delete(key);
      else state.collapsed.add(key);
    };
    const check = (key, on) => { state.sel.set(key, on); emit("change"); };
    const checkDir = (node, on) => {
      const walk = x => {
        for (const f of x.files) state.sel.set(f.path.join("/"), on);
        for (const d of x.dirs.values()) walk(d);
      };
      walk(node);
      emit("change");
    };

    // 目录复选框显示态:由后代文件勾选集推导(全勾=勾,部分=半选)
    const dirCheckState = node => {
      let n = 0, on = 0;
      const walk = x => {
        for (const f of x.files) { n++; if (state.sel.get(f.path.join("/"))) on++; }
        for (const d of x.dirs.values()) walk(d);
      };
      walk(node);
      return { checked: n > 0 && on === n, indeterminate: on > 0 && on < n };
    };

    function prioCell(done, size, wanted) {
      let txt, cls = "";
      if (wanted === false) { txt = "不下载"; cls = "skip"; }
      else if (size <= 0 || done >= size) { txt = "[完成]"; cls = "done"; }
      else if (props.taskStatus === "running" && done > 0) txt = "下载中";
      else txt = "普通";
      return h("span", { class: "ft-prio" + (cls ? " " + cls : "") }, txt);
    }
    function progCell(pct) {
      return h("span", { class: "ft-prog" }, [
        h("span", { class: "mini-prog" }, h("div", { style: { width: pct.toFixed(1) + "%" } })),
        h("span", { class: "pct2" }, pct.toFixed(0) + "%"),
      ]);
    }

    const nameCell = (title, iconHTML, nm, extra) => h("span", { class: "ft-name", title }, [
      CARET(),
      iconHTML ? h("span", { class: "folder-ic", innerHTML: iconHTML }) : h("span", { class: "ft-ic", innerHTML: iconHTML }),
      h("span", { class: "nm" }, nm),
      ...(extra || []),
    ]);

    function renderNode(node, depth) {
      const out = [];
      for (const [k, d] of node.dirs) {
        const agg = ftAgg(d);
        const pct = agg.size > 0 ? Math.min(100, agg.done / agg.size * 100) : (agg.count ? 100 : 0);
        const collapsed = state.collapsed.has(k);
        const cells = [];
        if (withCb()) {
          const cs = dirCheckState(d);
          cells.push(h("input", {
            type: "checkbox", class: "ft-cb ft-dir-cb", "data-dir": k,
            checked: cs.checked, indeterminate: cs.indeterminate,
            onChange: e => checkDir(d, e.target.checked),
          }));
        }
        cells.push(nameCell(k, ms("folder"), d.name, [h("span", { class: "ft-count" }, agg.count + " 项")]));
        cells.push(h("span", { class: "ft-size" }, fmtBytes(agg.size)));
        if (props.mode === "view") {
          cells.push(progCell(pct));
          cells.push(prioCell(agg.done, agg.size, agg.wanted));
        }
        cells.push(h("span", { class: "ft-range" }, ftPieceText(agg.first, agg.last)));
        out.push(h("div", {
          class: ["ft-row", "ft-dir", collapsed ? "collapsed" : ""],
          "data-dir": k,
          style: { paddingLeft: depth * 14 + "px" },
          onClick: e => { if (!e.target.closest("input")) toggleCollapse(k); },
        }, cells));
        out.push(h("div", { class: "ft-children", style: collapsed ? { display: "none" } : {} },
          renderNode(d, depth + 1)));
      }
      for (const f of node.files) {
        const fp = f.path.join("/");
        const pct = f.size > 0 ? Math.min(100, (f.done || 0) / f.size * 100) : 100;
        const cells = [];
        if (withCb()) {
          cells.push(h("input", {
            type: "checkbox", class: "ft-cb", "data-path": fp,
            checked: !!state.sel.get(fp),
            onChange: e => check(fp, e.target.checked),
          }));
        }
        cells.push(h("span", { class: "ft-name", title: fp }, [
          h("span", { class: "ft-ic", innerHTML: ms(fileIcon(f.path[f.path.length - 1])) }),
          h("span", { class: "nm" }, f.path[f.path.length - 1]),
        ]));
        cells.push(h("span", { class: "ft-size" }, fmtBytes(f.size)));
        if (props.mode === "view") {
          cells.push(progCell(pct));
          cells.push(prioCell(f.done || 0, f.size, f.wanted));
        }
        cells.push(h("span", { class: "ft-range" }, ftPieceText(f.first, f.last)));
        out.push(h("div", { class: "ft-row ft-file", key: fp, style: { paddingLeft: depth * 14 + 20 + "px" } }, cells));
      }
      return out;
    }

    return () => {
      const headCols = props.mode === "sel"
        ? [["", ""], ["文件名", "l"], ["大小", ""], ["分块序号", ""]]
        : [["文件名", "l"], ["大小", ""], ["进度", ""], ["优先级", ""], ["分块序号", ""]];
      const cols = props.mode === "view" && props.editable ? [["", ""], ...headCols] : headCols;
      return h("div", { class: ["ftable", props.mode, props.editable ? "editable" : ""] }, [
        h("div", { class: "ft-row ft-head" },
          cols.map(([t, a]) => h("span", a ? { class: a } : {}, t))),
        ...renderNode(tree.value, 0),
      ]);
    };
  },
};

</script>
