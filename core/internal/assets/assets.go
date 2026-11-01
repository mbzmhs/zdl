// Package assets 承载原生壳(cmd/zdl3)所需的静态资源 embed。
// go:embed 不能引用上级目录,故从 core/assets 与 webapp/src/assets 复制
// 副本;两处源头更新时需同步(打包里程碑把复制收进构建脚本)。
package assets

import (
	_ "embed"
)

//go:embed tray.png
var TrayIcon []byte

//go:embed app-icon.png
var AppIcon []byte

// IconsFont Material Symbols Rounded 子集(按 MS_ICONS 码位用 fonttools
// 裁剪;源=webapp/scripts/.cache 全量字体,新增图标后需重跑裁剪脚本同步)。
// 原生壳图标渲染走它:MS_ICONS 码位 + 该字体 = 与 Vue 版同一套字形。
//go:embed ms-icons.ttf
var IconsFont []byte

// UISans/UIBold 微软雅黑子集(GB2312 常用字+假名+全角,~1.8MB/权)。
// 原 =style.css 的 --font 栈(Segoe UI/YaHei):Fyne 只能挂单一字体源,
// 用雅黑统一中西文并取真粗体(系统 simhei 无粗体面,中文观感也偏硬)。
// 生成:python tools/genfonts.py(fonttools,源 C:\Windows\Fonts\msyh*.ttc)
//go:embed msyh-regular.ttf
var UISans []byte

//go:embed msyh-bold.ttf
var UIBold []byte
