//go:build darwin

// macOS 托盘:NSStatusItem(cgo,零第三方依赖)。左键/双击唤起主窗(与 Windows
// 同语义),右键弹菜单(打开管理界面/设置/退出,标签与 Windows 侧一致);
// Notify 走 osascript 系统通知中心。
// 线程模型:全部 UI 操作经 dispatch_async 派发到 Cocoa 主线程(Wails 事件循环
// 所在线程,其 NSApplication run loop 会清空 main queue);菜单/按钮动作回调经
// //export 桥回 Go 后立即转独立 goroutine,不阻塞菜单跟踪与主循环。
// ObjC 实现拆在 tray_darwin.h/.m:带 //export 的包其 cgo 前导码被编译两遍,
// 实现写在前导码里会重复符号,前导码只留声明。
package tray

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa

#import "tray_darwin.h"
*/
import "C"

import (
	_ "embed"
	"os/exec"
	"strings"
	"sync"
	"unsafe"
)

// macTrayGlyph 菜单栏专用 Z 字形模板图标(36px=18pt@2x,黑字+alpha)。
// template 渲染只取 alpha 通道,故无需从品牌图标转换——直接嵌入专用字形,
// 避免转换法把图标圆角底的高光边界漏成白边(见 scripts/genicns 同源说明)。
//
//go:embed trayglyph.png
var macTrayGlyph []byte

var (
	trayMu     sync.Mutex
	openFn     func()
	settingsFn func()
	exitFn     func()
)

// Run 创建状态栏托盘项:创建动作异步派发到主线程,本函数立即返回,不阻塞调用方。
func Run(iconPNG []byte, tooltip string, open, openSettings, exit func()) error {
	trayMu.Lock()
	openFn, settingsFn, exitFn = open, openSettings, exit
	trayMu.Unlock()

	// 菜单栏用专用 Z 字形模板(36px=18pt@2x);iconPNG 仅作「是否显示图标」的
	// 开关(Windows 侧同一入口用品牌彩色图标),darwin 不从品牌图做转换——
	// 转换法会把图标圆角底的高光边界漏成白边。
	if len(iconPNG) > 0 {
		iconPNG = macTrayGlyph
	}
	// C.CBytes/C.CString 产生 C 堆内存,所有权移交 zdTraySetupC(块内释放)
	cIcon := C.CBytes(iconPNG)
	cTip := C.CString(tooltip)
	C.zdTraySetupC((*C.char)(cIcon), C.int(len(iconPNG)), cTip)
	return nil
}

// Close 移除状态栏项(异步派发主线程)。
func Close() {
	C.zdTrayShutdownC()
}

// Notify 弹系统通知(osascript display notification),失败静默。
func Notify(title, text string) {
	_ = exec.Command("osascript", "-e", "display notification "+asl(text)+" with title "+asl(title)).Run()
}

// asl 把字符串渲染为 AppleScript 字符串字面量(反斜杠/双引号/换行转义)。
func asl(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
	)
	return `"` + r.Replace(s) + `"`
}

//export zdTrayOpen
func zdTrayOpen() {
	trayMu.Lock()
	fn := openFn
	trayMu.Unlock()
	if fn != nil {
		go fn() // 脱离主线程回调栈:菜单立即收起,主循环不被 ShowUI 的置顶延时阻塞
	}
}

//export zdTraySettings
func zdTraySettings() {
	trayMu.Lock()
	fn := settingsFn
	trayMu.Unlock()
	if fn != nil {
		go fn()
	}
}

//export zdTrayExit
func zdTrayExit() {
	trayMu.Lock()
	fn := exitFn
	trayMu.Unlock()
	if fn != nil {
		go fn()
	}
}

// 确保 unsafe 被使用(cIcon 转换)
var _ = unsafe.Pointer(nil)
