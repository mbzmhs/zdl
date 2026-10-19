// genico 把 assets/app.ico 以资源组 ID 3 嵌入 Windows .syso 目标文件,
// 供 go build 自动链入 exe —— wails 按 winc.AppIconID(=3)加载
// 窗口/任务栏图标,Explorer 也用第一个图标资源显示 exe 文件图标。
// 独立 go.mod:这是构建期工具,依赖不进核心模块,保持核心零第三方依赖。
// 用法:在 core/ 目录下 `go run ./scripts/genico`(app.ico 由 genicons 生成)。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/tc-hib/winres"
)

const appIconResourceID = 3 // wails v2 winc.AppIconID

// coreDir 定位 core/ 目录,与调用时的 cwd 无关
func coreDir() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	// scripts/genico/main.go → scripts → core
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

func main() {
	core := coreDir()
	icoPath := filepath.Join(core, "assets", "app.ico")
	if len(os.Args) > 1 {
		icoPath = os.Args[1]
	}
	f, err := os.Open(icoPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开图标失败(先运行 genicons 生成):", err)
		os.Exit(1)
	}
	defer f.Close()

	icon, err := winres.LoadICO(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "解析 ICO 失败:", err)
		os.Exit(1)
	}

	rs := winres.ResourceSet{}
	if err := rs.SetIcon(winres.ID(appIconResourceID), icon); err != nil {
		fmt.Fprintln(os.Stderr, "设置图标资源失败:", err)
		os.Exit(1)
	}
	outPath := filepath.Join(core, "rsrc_windows_amd64.syso")
	out, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "创建 .syso 失败:", err)
		os.Exit(1)
	}
	defer out.Close()
	if err := rs.WriteObject(out, winres.ArchAMD64); err != nil {
		fmt.Fprintln(os.Stderr, "写出 .syso 失败:", err)
		os.Exit(1)
	}
	fmt.Println("generated", outPath, "from", icoPath)
}
