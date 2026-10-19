// 文件树纯函数(逐行对齐旧 app.js):BitComet 式目录聚合 + 填充文件识别

// BitComet 0.85+ 制种用 ____padding_file_N_ 命名的填充文件做文件对齐,
// BEP-47 规范形态是 .pad/<字节数>;属种子正常成员但展示无意义
// (引擎侧对齐段落读零写丢,盘上不会出现这些文件)
const PADDING_FILE_RE = /^_{2,}padding_file_\d+_/i;
export const isPaddingFile = f => PADDING_FILE_RE.test(f.path[f.path.length - 1] || "") ||
  (f.path || []).some(seg => seg === ".pad");

// 按扩展名挑文件图标(取文件名末段;目录行固定 folder)
const FILE_ICON_RULES = [
  [/^(mp4|mkv|avi|mov|wmv|flv|ts|m2ts|webm|m4v|rmvb|mpg|mpeg|3gp|vob)$/, "movie"],
  [/^(mp3|flac|ape|m4a|aac|wav|ogg|opus|wma|mid|tak|dsf)$/, "music_note"],
  [/^(jpg|jpeg|png|gif|bmp|webp|svg|heic|avif|tiff|ico|psd)$/, "image"],
  [/^(zip|rar|7z|tar|gz|bz2|xz|zst|iso|cab)$/, "folder_zip"],
  [/^pdf$/, "picture_as_pdf"],
  [/^(srt|ass|ssa|sub|idx|sup)$/, "subtitles"],
  [/^(json|yml|yaml|xml|html|htm|css|js|mjs|ts|jsx|tsx|py|go|c|h|cpp|hpp|java|rs|sh|bat|cmd|ps1|sql)$/, "code"],
  [/^(xls|xlsx|csv|ods)$/, "table_chart"],
  [/^(exe|msi|dll|sys|apk|appimage|deb|rpm)$/, "terminal"],
  [/^(txt|md|nfo|log|doc|docx|odt|rtf)$/, "description"],
];
export function fileIcon(name) {
  const ext = (String(name).split(".").pop() || "").toLowerCase();
  for (const [re, icon] of FILE_ICON_RULES) if (re.test(ext)) return icon;
  return "note";
}

export function buildFileTree(files) {
  const root = { dirs: new Map(), files: [] };
  for (const f of files) {
    let node = root, acc = [];
    for (let i = 0; i < f.path.length - 1; i++) {
      acc.push(f.path[i]);
      const k = acc.join("/");
      let ch = node.dirs.get(k);
      if (!ch) { ch = { name: f.path[i], dirs: new Map(), files: [] }; node.dirs.set(k, ch); }
      node = ch;
    }
    node.files.push(f);
  }
  return root;
}

// 目录聚合:大小/完成字节/文件数 + 覆盖的 piece 区间(分块序号列)。
// wanted:全部未选=false / 有选中=undefined(混合,优先级列显示"普通")/全部选中=true。
// wanted 一律取服务端数据(f.wanted),与旧版一致:勾选改动不即时改优先级列,
// 应用更改后经服务端快照回填。
export function ftAgg(n) {
  let done = 0, size = 0, count = 0, first = Infinity, last = -1, wAny = false, wAll = true;
  const walk = x => {
    for (const f of x.files) {
      done += f.done || 0; size += f.size || 0; count++;
      if (f.last >= f.first) { first = Math.min(first, f.first); last = Math.max(last, f.last); }
      if (f.wanted === false) wAll = false; else wAny = true;
    }
    for (const d of x.dirs.values()) walk(d);
  };
  walk(n);
  return { done, size, count, first: count ? first : 0, last, wanted: wAll && wAny ? true : (wAny ? undefined : false) };
}

export function ftPieceText(first, last) {
  if (!(last >= first)) return "—"; // 零长文件/空目录
  return first === last ? String(first) : `${first}~${last}`;
}

// 优先级列(对照 BitComet 下载优先级):[完成] / 不下载 / 下载中 / 普通
export function ftPrioTxt(done, size, wanted, running) {
  if (wanted === false) return { txt: "不下载", cls: "skip" };
  if (size <= 0 || done >= size) return { txt: "[完成]", cls: "done" };
  if (running && done > 0) return { txt: "下载中", cls: "" };
  return { txt: "普通", cls: "" };
}
