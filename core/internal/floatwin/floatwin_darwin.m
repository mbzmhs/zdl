// floatwin_darwin.m — macOS 悬浮窗(NSPanel + WKWebView)实现,被 floatwin_darwin.go 引用。
#import "floatwin_darwin.h"
#import <AppKit/AppKit.h>
#import <dispatch/dispatch.h>

static NSPanel *zdFloatPanel = nil;
static WKWebView *zdFloatWebView = nil;
static NSObject<NSWindowDelegate, WKNavigationDelegate, WKScriptMessageHandler> *zdFloatDelegate = nil;
static NSMenu *zdFloatMenu = nil;
// 原生拖拽状态(见 ZDFloatDelegate startNativeDrag)
static NSPoint zdDragGrabOffset;   // 光标 → 面板 origin 的抓取偏移
static id zdDragMonitor = nil;     // 左键拖拽/抬起的本地事件监视器
static BOOL zdNativeDragging = NO;

// 默认尺寸/位置:右上角贴菜单栏下方(位置记忆无效时用)
static NSSize zdFloatDefaultSize(void) { return NSMakeSize(288, 142); }

// 钳进单一屏幕的可见区:目标屏取面板中心所在屏(在屏间隙则取最近屏)。
// 不允许面板骑在交界上——骑界时 window 的 backing scale 随两屏反复切换,
// 透明 WKWebView 每次切换都重栅格化一帧,视觉上就是交界处疯狂闪烁(实测);
// 整体钳进单屏后 scale 只在跨屏瞬间切换一次。顺带保证面板永不被拖丢。
static NSPoint zdClampedOrigin(NSPoint o) {
    NSRect f = zdFloatPanel.frame;
    NSPoint center = NSMakePoint(o.x + f.size.width / 2, o.y + f.size.height / 2);
    NSScreen *target = nil;
    CGFloat best = CGFLOAT_MAX;
    for (NSScreen *s in [NSScreen screens]) {
        NSRect fr = s.frame;
        if (NSMouseInRect(center, fr, NO)) { target = s; break; }
        // 屏间隙:按到屏框中心的距离取最近
        CGFloat dx = MAX(MAX(fr.origin.x - center.x, center.x - (fr.origin.x + fr.size.width)), 0);
        CGFloat dy = MAX(MAX(fr.origin.y - center.y, center.y - (fr.origin.y + fr.size.height)), 0);
        CGFloat d = dx * dx + dy * dy;
        if (d < best) { best = d; target = s; }
    }
    if (target == nil) { return o; }
    NSRect v = target.visibleFrame;
    o.x = MIN(MAX(o.x, v.origin.x), v.origin.x + v.size.width - f.size.width);
    o.y = MIN(MAX(o.y, v.origin.y), v.origin.y + v.size.height - f.size.height);
    return o;
}

@interface ZDFloatDelegate : NSObject <NSWindowDelegate, WKNavigationDelegate, WKScriptMessageHandler>
- (void)openUI:(id)sender;
- (void)openSettings:(id)sender;
- (void)quitApp:(id)sender;
@end
@implementation ZDFloatDelegate
// 拖拽分两段:JS mousedown 后的首条 drag 消息只当「拖拽起点」信号,此后位置
// 完全由原生事件驱动——以全局光标坐标(NSEvent mouseLocation,与 webview
// 坐标系无关)减抓取偏移定位。原先 JS 增量(e.screenX 差值)+ setFrameOrigin
// 是个反馈环:跨屏(backing scale 变化)时 webview 上报的 screen 坐标跳变,
// dx/dy 出尖峰,面板乱跳又反过来让下一次 screen 读数更歪——实测跨屏拖动
// 疯狂闪烁乱移动。拖拽期间本地监视器吞掉拖拽事件(不再回喂 JS),windowDidMove
// 静默,松手才回存一次位置。
- (void)startNativeDrag {
    if (zdNativeDragging || zdFloatPanel == nil) { return; }
    NSPoint cursor = [NSEvent mouseLocation];
    zdDragGrabOffset = NSMakePoint(cursor.x - zdFloatPanel.frame.origin.x,
                                   cursor.y - zdFloatPanel.frame.origin.y);
    zdNativeDragging = YES;
    zdDragMonitor = [NSEvent addLocalMonitorForEventsMatchingMask:
        NSEventMaskLeftMouseDragged | NSEventMaskLeftMouseUp
        handler:^NSEvent *(NSEvent *e) {
            if (e.type == NSEventTypeLeftMouseUp) {
                [self endNativeDrag];
                return e; // 放行抬起事件,让 JS 归位自己的 drag 状态
            }
            NSPoint o = zdClampedOrigin(NSMakePoint(
                [NSEvent mouseLocation].x - zdDragGrabOffset.x,
                [NSEvent mouseLocation].y - zdDragGrabOffset.y));
            [zdFloatPanel setFrameOrigin:o];
            return nil; // 吞掉拖拽事件:webview 不再产生 JS 增量消息
        }];
}
- (void)endNativeDrag {
    if (!zdNativeDragging) { return; }
    if (zdDragMonitor != nil) { [NSEvent removeMonitor:zdDragMonitor]; zdDragMonitor = nil; }
    zdNativeDragging = NO;
    if (zdFloatPanel == nil) { return; }
    NSRect f = zdFloatPanel.frame;
    zdFloatOnMove((int)f.origin.x, (int)(f.origin.y + f.size.height), (int)f.size.width, (int)f.size.height);
}
- (void)userContentController:(WKUserContentController *)userContentController
      didReceiveScriptMessage:(WKScriptMessage *)message {
    if (![message.name isEqualToString:@"zdfloat"] || zdFloatPanel == nil) { return; }
    NSDictionary *d = (NSDictionary *)message.body;
    if (![d[@"act"] isEqualToString:@"drag"]) { return; }
    if ([d[@"end"] boolValue]) {
        [self endNativeDrag]; // JS 抬起收尾(原生监视器已先收尾则幂等)
        return;
    }
    if (zdNativeDragging) { return; } // 原生拖拽进行中:JS 增量一律忽略
    [self startNativeDrag];
}
// 程序性 setFrameOrigin(原生拖拽/调高)同样会走到这里;原生拖拽期间的回存
// 由 endNativeDrag 统一做,这里跳过避免逐事件 HTTP PUT
- (void)windowDidMove:(NSNotification *)notification {
    if (zdFloatPanel == nil || zdNativeDragging) { return; }
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
