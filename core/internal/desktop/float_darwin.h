// float_darwin.h — macOS 悬浮窗 cgo 桥接声明(实现见 float_darwin.m)。
// 带导出函数的包其 cgo 前导码被编译两遍,实现必须拆独立 .h/.m(托盘同款教训)。
#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#import <stdlib.h>

// Go 侧回调(cgo //export 生成)
extern void zdFloatOnMove(int x, int y, int w, int h);
extern void zdFloatOnOpen(void);
extern void zdFloatMenuOpen(void);
extern void zdFloatMenuSettings(void);
extern void zdFloatMenuExit(void);

// 系统即将关机/重启通知到达时的回调(主线程;Go 侧 //export 生成)
extern void zdAppOnSystemShutdown(void);

// 切换 NSApplication 激活策略(主线程,异步按调用顺序生效):
// regular=1 → NSApplicationActivationPolicyRegular,显示 Dock 图标并出现在应用切换器;
// regular=0 → NSApplicationActivationPolicyAccessory,无 Dock 图标、不出现在应用切换器,
// 但窗口/面板/托盘照常工作(隐藏到托盘时用——关闭后不残留 Dock 图标,唤起时再出现)。
void zdAppSetActivationPolicyC(int regular);

// 主线程创建面板并加载 url(x/y/w/h = 记忆位置,w/h 为 0 用默认;posSet=是否记忆有效)
void zdFloatCreateC(const char *url, int x, int y, int w, int h, int posSet);

// 主线程显隐(1=显示 0=隐藏);面板未创建时为空操作
void zdFloatSetVisibleC(int on);

// 主线程同步收起悬浮窗(退出流程用)
void zdFloatCloseC(void);

// 退出辅助(主线程同步执行):orderOut 应用全部窗口 + NSApp stop + post 哑事件。
// wails 的 Quit 仅 [NSApp stop:](stop 要等下一个事件才生效,应用闲置时主窗会
// 残留数秒);本函数让主窗立即消失且事件循环立即退出。
void zdAppQuitAssistC(void);

// 注册 NSWorkspace 关机/重启观察(主线程调用,事件循环启动前/后均可):
// 系统关机/重启前会向所有 GUI 应用送达 WillPowerOff/WillRestart 通知。
// Wails v2 的 applicationShouldTerminate 恒返回 NSTerminateCancel(只把 "Q"
// 转给 Go 侧),系统只能等应用自己退出——不主动退出即被强杀并弹
// "ZDownloader 中断了关机"。通知到达时回调 zdAppOnSystemShutdown。
void zdAppRegisterShutdownObserverC(void);
