//go:build darwin

// 托盘/单实例转交唤起时的 macOS 置前两件套:
//   - activateApp:把应用带到前台并取回键盘焦点。macOS 14+ 对没有直接用户
//     交互背书的后台应用会「缓办」activateIgnoringOtherApps——窗口先落在
//     当前前台应用后面,数秒后系统才完成激活(实测 ~5s)。
//   - raiseWindowsToFront:不依赖激活的兜底——窗级闪动(置顶一拍再回常态),
//     立即改写 Z 序把窗口提到最前(1.8 wails 壳 showToScreen 的置顶闪动同款;
//     悬浮窗 NSPanel 的 orderFrontRegardless 同理)。两者都调:激活成功则有
//     焦点,激活被缓办窗口也已可见。
//
// 须在 Fyne 主线程调用(fyne.Do 块内),与 AppKit 主线程规则一致。
package nativeui

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa

#import <Cocoa/Cocoa.h>

static void zdUIActivateApp(void) {
    [NSApp activateIgnoringOtherApps:YES];
}

static void zdUIRaiseWindows(void) {
    for (NSWindow *w in [NSApp windows]) {
        if (![w isVisible]) { continue; }              // 只动可见窗,不把隐藏窗意外上屏
        if ([w isKindOfClass:[NSPanel class]]) { continue; } // 悬浮窗面板有自己的显隐节奏
        [w setLevel:NSFloatingWindowLevel];
        [w orderFrontRegardless];                       // 立即到最前,不抢键盘焦点
        [w setLevel:NSNormalWindowLevel];               // 回常态,保持刚拿到的 Z 序
    }
}
*/
import "C"

func activateApp() { C.zdUIActivateApp() }

func raiseWindowsToFront() { C.zdUIRaiseWindows() }
