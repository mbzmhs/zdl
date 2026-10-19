// float_darwin.m — macOS 悬浮窗(NSPanel + WKWebView)实现,被 float_darwin.go 引用。
#import "float_darwin.h"
#import <AppKit/AppKit.h>
#import <dispatch/dispatch.h>

static NSPanel *zdFloatPanel = nil;
static WKWebView *zdFloatWebView = nil;
static NSObject<NSWindowDelegate, WKNavigationDelegate, WKScriptMessageHandler> *zdFloatDelegate = nil;
static NSMenu *zdFloatMenu = nil;

// 默认尺寸/位置:右上角贴菜单栏下方(位置记忆无效时用)
static NSSize zdFloatDefaultSize(void) { return NSMakeSize(288, 142); }

@interface ZDFloatDelegate : NSObject <NSWindowDelegate, WKNavigationDelegate, WKScriptMessageHandler>
- (void)openUI:(id)sender;
- (void)openSettings:(id)sender;
- (void)quitApp:(id)sender;
@end
@implementation ZDFloatDelegate
// JS 拖拽:{act:"drag", dx, dy, end} — dx 向右/dy 向下为正(屏幕坐标),
// cocoa 的 y 轴向上,故 origin.y -= dy;end=true 时回存位置。
// 不用 movableByWindowBackground:WKWebView 自消费鼠标事件,该机制对它无效。
- (void)userContentController:(WKUserContentController *)userContentController
      didReceiveScriptMessage:(WKScriptMessage *)message {
    if (![message.name isEqualToString:@"zdfloat"] || zdFloatPanel == nil) { return; }
    NSDictionary *d = (NSDictionary *)message.body;
    if (![d[@"act"] isEqualToString:@"drag"]) { return; }
    CGFloat dx = [d[@"dx"] doubleValue];
    CGFloat dy = [d[@"dy"] doubleValue];
    NSRect f = zdFloatPanel.frame;
    f.origin.x += dx;
    f.origin.y -= dy;
    [zdFloatPanel setFrameOrigin:f.origin];
    if ([d[@"end"] boolValue]) {
        zdFloatOnMove((int)f.origin.x, (int)(f.origin.y + f.size.height), (int)f.size.width, (int)f.size.height);
    }
}
// 拖动结束回存位置(左上角 + 宽高,Cocoa 全局坐标)
- (void)windowDidMove:(NSNotification *)notification {
    if (zdFloatPanel == nil) { return; }
    NSRect f = zdFloatPanel.frame;
    zdFloatOnMove((int)f.origin.x, (int)(f.origin.y + f.size.height), (int)f.size.width, (int)f.size.height);
}
// 页面内 zd-float:// 导航 → 原生动作(无 JS 桥的轻量 IPC):
// open=双击唤起主窗;menu?x&y=右键在光标处弹菜单(页面传 client 坐标,
// 此处换算屏幕坐标);h?N=内容高度上报(N px,顶边锚定调高,对应 Windows
// floatH=0 随行数语义)。
- (void)webView:(WKWebView *)webView decidePolicyForNavigationAction:(WKNavigationAction *)navigationAction
    decisionHandler:(void (^)(WKNavigationActionPolicy))decisionHandler {
    NSString *url = navigationAction.request.URL.absoluteString;
    if ([url hasPrefix:@"zd-float://open"]) {
        zdFloatOnOpen();
        decisionHandler(WKNavigationActionPolicyCancel);
        return;
    }
    if ([url hasPrefix:@"zd-float://menu"]) {
        [self showMenuAt:url];
        decisionHandler(WKNavigationActionPolicyCancel);
        return;
    }
    if ([url hasPrefix:@"zd-float://h?"]) {
        int h = [url substringFromIndex:@"zd-float://h?".length].intValue;
        if (h > 40 && h < 800) { [self resizeFloatTo:h]; }
        decisionHandler(WKNavigationActionPolicyCancel);
        return;
    }
    decisionHandler(WKNavigationActionPolicyAllow);
}
// 右键菜单弹在光标处:url 携带页面 client 坐标(左上原点),换算到屏幕
// 坐标(cocoa 左下原点):x = 面板左 + clientX,y = 面板顶 - clientY。
// 无坐标参数(旧页面)时退回面板右下角。异步弹出:等发起导航的事件流收尾,
// 避免菜单被重放事件秒关。
- (void)showMenuAt:(NSString *)url {
    if (zdFloatPanel == nil) { return; }
    NSURLComponents *c = [NSURLComponents componentsWithString:url];
    CGFloat cx = -1, cy = -1;
    for (NSURLQueryItem *q in c.queryItems) {
        if ([q.name isEqualToString:@"x"]) { cx = q.value.doubleValue; }
        if ([q.name isEqualToString:@"y"]) { cy = q.value.doubleValue; }
    }
    NSRect f = zdFloatPanel.frame;
    NSPoint p;
    if (cx >= 0 && cy >= 0) {
        p = NSMakePoint(f.origin.x + cx, f.origin.y + f.size.height - cy);
    } else {
        p = NSMakePoint(f.origin.x + f.size.width - 8, f.origin.y - 6);
    }
    dispatch_async(dispatch_get_main_queue(), ^{
        [zdFloatMenu popUpMenuPositioningItem:nil atLocation:p inView:nil];
    });
}
// 顶边锚定调高并回存位置(windowDidMove 不覆盖 resize,主动报一次)。
// 锚定符号:cocoa origin 是左下角,保持顶边不动 → origin.y -= (新高-旧高)。
// 写成 += 会让窗口在收起/展开瞬间整体下/上跳,跳出光标下方(按钮"集体失效"假象)。
- (void)resizeFloatTo:(CGFloat)newH {
    if (zdFloatPanel == nil) { return; }
    NSRect f = zdFloatPanel.frame;
    CGFloat dh = newH - f.size.height;
    if (dh == 0) { return; }
    f.origin.y -= dh;
    f.size.height = newH;
    [zdFloatPanel setFrame:f display:YES];
    zdFloatOnMove((int)f.origin.x, (int)(f.origin.y + f.size.height), (int)f.size.width, (int)f.size.height);
}
- (void)openUI:(id)sender       { zdFloatMenuOpen(); }
- (void)openSettings:(id)sender { zdFloatMenuSettings(); }
- (void)quitApp:(id)sender      { zdFloatMenuExit(); }
@end

void zdFloatCreateC(const char *url, int x, int y, int w, int h, int posSet) {
    dispatch_async(dispatch_get_main_queue(), ^{
        if (zdFloatPanel != nil) { free((void *)url); return; }

        [NSApplication sharedApplication];
        zdFloatDelegate = [[ZDFloatDelegate alloc] init];

        NSSize def = zdFloatDefaultSize();
        CGFloat fw = w > 0 ? w : def.width;
        CGFloat fh = h > 0 ? h : def.height;
        NSRect frame = NSMakeRect(0, 0, fw, fh);
        if (posSet) {
            // 记忆位置:左上角(x, y)为 Cocoa 全局坐标
            frame.origin.x = x;
            frame.origin.y = y - fh;
        } else {
            NSScreen *main = NSScreen.mainScreen;
            NSRect mf = main.frame;
            frame.origin.x = mf.origin.x + mf.size.width - fw - 24;
            frame.origin.y = mf.origin.y + mf.size.height - fh - 32; // 菜单栏下方
        }

        zdFloatPanel = [[NSPanel alloc]
            initWithContentRect:frame
                      styleMask:NSWindowStyleMaskBorderless | NSWindowStyleMaskNonactivatingPanel
                        backing:NSBackingStoreBuffered
                          defer:NO];
        // 非 ARC:dispatch 块级 autorelease pool 块尾排空,静态持有必须显式 retain
        [zdFloatPanel retain];
        zdFloatPanel.level = NSFloatingWindowLevel; // 置顶但不压菜单栏
        zdFloatPanel.opaque = NO;                   // 透明面板 + 页面圆角
        zdFloatPanel.backgroundColor = [NSColor clearColor];
        zdFloatPanel.hasShadow = YES;
        // 拖拽由 JS mousemove + script message 桥驱动(见 ZDFloatDelegate),
        // movableByWindowBackground 对 WKWebView 无效,保留为无害冗余
        zdFloatPanel.movableByWindowBackground = YES;
        zdFloatPanel.ignoresMouseEvents = NO;
        zdFloatPanel.collectionBehavior = NSWindowCollectionBehaviorCanJoinAllSpaces |
                                          NSWindowCollectionBehaviorFullScreenAuxiliary;
        zdFloatPanel.delegate = zdFloatDelegate;

        WKWebViewConfiguration *cfg = [[WKWebViewConfiguration alloc] init];
        [cfg.userContentController addScriptMessageHandler:zdFloatDelegate name:@"zdfloat"];
        zdFloatWebView = [[WKWebView alloc] initWithFrame:zdFloatPanel.contentView.bounds
                                            configuration:cfg];
        [zdFloatWebView retain];
        // 透明底:页面自身的圆角深色背景可见,避免白底直角
        [zdFloatWebView setValue:@NO forKey:@"drawsBackground"];
        zdFloatWebView.navigationDelegate = zdFloatDelegate;
        zdFloatPanel.contentView = zdFloatWebView;
        // 原生层圆角裁剪,必须设在 webview 自己的 layer 上(设在默认 contentView
        // 会被上面的 contentView 赋值整个换掉、配置随之丢失——bt.189 踩坑):
        // 页面内 border-radius 在窗口 resize 后角落重绘滞后,会残留直角旧帧;
        // CA 层 masksToBounds 在合成期强制裁剪,与重绘无关
        zdFloatWebView.wantsLayer = YES;
        zdFloatWebView.layer.cornerRadius = 10;
        zdFloatWebView.layer.masksToBounds = YES;

        // 右键菜单(与托盘同款三项目标语义)
        zdFloatMenu = [[NSMenu alloc] init];
        zdFloatMenu.autoenablesItems = NO;
        NSMenuItem *it = [zdFloatMenu addItemWithTitle:@"打开管理界面" action:@selector(openUI:) keyEquivalent:@""];
        it.target = zdFloatDelegate;
        it = [zdFloatMenu addItemWithTitle:@"设置" action:@selector(openSettings:) keyEquivalent:@""];
        it.target = zdFloatDelegate;
        [zdFloatMenu addItem:NSMenuItem.separatorItem];
        it = [zdFloatMenu addItemWithTitle:@"退出" action:@selector(quitApp:) keyEquivalent:@""];
        it.target = zdFloatDelegate;

        // 运行时 char* 须 stringWithUTF8String:(@() 装箱仅限编译期字面量)
        NSString *nsURL = [NSString stringWithUTF8String:url];
        [zdFloatWebView loadRequest:[NSURLRequest requestWithURL:[NSURL URLWithString:nsURL]]];

        [zdFloatPanel orderFrontRegardless]; // 上屏不抢焦点
        free((void *)url);
    });
}

// zdAppQuitAssistC 退出辅助,主线程同步执行:全部窗口 orderOut(主窗立即消失,
// 不依赖 wails WindowHide 的异步时序)+ NSApp stop + post 哑事件(atStart)。
// [NSApp stop:] 语义是"处理完下一个事件才退出"——应用闲置无事件时循环一直
// 转着,主窗/托盘收了但循环迟迟不退;哑事件就是那个"下一个事件"。
void zdAppQuitAssistC(void) {
    dispatch_sync(dispatch_get_main_queue(), ^{
        for (NSWindow *w in [NSApp windows]) {
            [w orderOut:nil];
        }
        [NSApp stop:nil];
        NSEvent *wake = [NSEvent otherEventWithType:NSEventTypeApplicationDefined
                                           location:NSMakePoint(0, 0)
                                      modifierFlags:0
                                          timestamp:0
                                       windowNumber:0
                                            context:nil
                                            subtype:0
                                              data1:0
                                              data2:0];
        [NSApp postEvent:wake atStart:YES];
    });
}

// zdAppRegisterShutdownObserverC 观察系统关机/重启:
// macOS 关机/重启前,NSWorkspace 向所有 GUI 应用(与窗口是否可见无关)送达
// WillPowerOff/WillRestart 通知,这是"系统要求退出"的可靠信号。
// Wails v2 的 applicationShouldTerminate 恒返回 NSTerminateCancel——系统侧
// 的退出请求被拒,只能等应用自己退出;托盘常驻模式下应用只是隐藏到托盘、
// 永远不退出,宽限期一到即被强杀,弹出"ZDownloader 中断了关机"。
// 收到通知即回调 Go 侧走与 SIGTERM 相同的优雅退出流程(根上下文取消 →
// 后台保存任务状态 → 进程退出),在系统宽限期内完成。
// 睡眠(WillSleep)无需处理:系统不要求应用退出即可睡眠。
static id zdShutdownObserverPowerOff = nil;
static id zdShutdownObserverRestart = nil;

void zdAppRegisterShutdownObserverC(void) {
    NSNotificationCenter *nc = [[NSWorkspace sharedWorkspace] notificationCenter];
    // 关机:公开常量。
    zdShutdownObserverPowerOff = [nc addObserverForName:NSWorkspaceWillPowerOffNotification
                                                  object:nil queue:nil
                                              usingBlock:^(NSNotification *note) {
        zdAppOnSystemShutdown();
    }];
    [zdShutdownObserverPowerOff retain]; // 非 ARC:块观察者须显式持有
    // 重启:WillRestart 是私有通知,公开 SDK 头无常量;NSWorkspace 通知常量
    // 的取值即其名字符串,用裸名匹配运行时送达的通知。
    zdShutdownObserverRestart = [nc addObserverForName:@"NSWorkspaceWillRestartNotification"
                                                 object:nil queue:nil
                                             usingBlock:^(NSNotification *note) {
        zdAppOnSystemShutdown();
    }];
    [zdShutdownObserverRestart retain];
}

void zdFloatSetVisibleC(int on) {
    dispatch_async(dispatch_get_main_queue(), ^{
        if (zdFloatPanel == nil) { return; }
        if (on) {
            [zdFloatPanel orderFrontRegardless];
        } else {
            [zdFloatPanel orderOut:nil];
        }
    });
}

// zdFloatCloseC 主线程同步收起悬浮窗:退出流程在主循环存活期调用(先于
// desk.Quit),悬浮窗与托盘图标、主窗一起立即消失,后台收尾期间不留残影。
void zdFloatCloseC(void) {
    dispatch_sync(dispatch_get_main_queue(), ^{
        if (zdFloatPanel != nil) {
            [zdFloatPanel orderOut:nil];
        }
    });
}
