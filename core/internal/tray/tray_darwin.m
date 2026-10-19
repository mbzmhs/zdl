// tray_darwin.m — macOS 托盘(NSStatusItem)实现,被 tray_darwin.go cgo 前导码引用。
#import "tray_darwin.h"
#import <dispatch/dispatch.h>
#import <stdlib.h>

@interface ZDTrayTarget : NSObject
- (void)openUI:(id)sender;
- (void)openSettings:(id)sender;
- (void)quitApp:(id)sender;
@end

static ZDTrayTarget *zdTrayTarget = nil;
static NSStatusItem *zdStatusItem = nil;
static NSMenu *zdTrayMenu = nil;

@implementation ZDTrayTarget
// 左右键都弹原生菜单(statusItem.menu 承载),不在按钮上挂动作:
// "sendActionOn 动作派发 + menu 跟踪"混用在快速点击时动作与跟踪会话交错
// 重入直接崩溃(bt.187.3 实测),纯原生菜单跟踪则天然稳定;打开主窗走
// 菜单第一项,与需求一致。
- (void)openUI:(id)sender       { zdTrayOpen(); }
- (void)openSettings:(id)sender { zdTraySettings(); }
- (void)quitApp:(id)sender      { zdTrayExit(); }
@end

void zdTraySetupC(const char *icon, int iconLen, const char *tooltip) {
    dispatch_async(dispatch_get_main_queue(), ^{
        if (zdStatusItem != nil) { free((void *)icon); free((void *)tooltip); return; }

        [NSApplication sharedApplication];

        if (zdTrayTarget == nil) { zdTrayTarget = [[ZDTrayTarget alloc] init]; }

        NSImage *img = nil;
        if (iconLen > 0) {
            NSData *data = [NSData dataWithBytes:icon length:(NSUInteger)iconLen];
            img = [[NSImage alloc] initWithData:data];
        }
        if (img != nil) {
            img.size = NSMakeSize(18, 18);
            img.template = YES; // 菜单栏随深浅色模式自动反色
        }

        zdStatusItem = [[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength];
        // 非 ARC:statusItemWithLength 返回 autoreleased 对象,而本块由 dispatch
        // 派发(块级 autorelease pool 在块尾排空),不显式 retain 则状态栏项
        // 创建后立即销毁——症状正是"日志已创建、菜单栏永远不显示"。
        [zdStatusItem retain];
        NSStatusBarButton *button = zdStatusItem.button;
        if (img != nil) { button.image = img; }
        else { button.title = @"ZD"; } // 图标解码失败兜底
        button.toolTip = [NSString stringWithUTF8String:tooltip];
        // 不设 target/action/sendActionOn:左右键都交给 statusItem.menu 原生跟踪,
        // 无动作派发即无"动作与跟踪会话重入"崩溃路径

        zdTrayMenu = [[NSMenu alloc] init];
        // autoenablesItems=NO:菜单项无条件可用。默认 YES 时逐项走校验
        // (respondsToSelector/响应链),状态栏菜单在非激活应用上下文里校验
        // 语义不稳,曾出现整菜单禁用;action 已绑定显式 target,无需自动校验。
        zdTrayMenu.autoenablesItems = NO;
        // addItemWithTitle:action:keyEquivalent: 只设 action 不设 target,
        // nil target 走响应链派发 → 点了没人应;必须逐项显式指回 ZDTrayTarget。
        NSMenuItem *it = [zdTrayMenu addItemWithTitle:@"打开管理界面" action:@selector(openUI:) keyEquivalent:@""];
        it.target = zdTrayTarget;
        it = [zdTrayMenu addItemWithTitle:@"设置" action:@selector(openSettings:) keyEquivalent:@""];
        it.target = zdTrayTarget;
        [zdTrayMenu addItem:NSMenuItem.separatorItem];
        it = [zdTrayMenu addItemWithTitle:@"退出" action:@selector(quitApp:) keyEquivalent:@""];
        it.target = zdTrayTarget;
        zdStatusItem.menu = zdTrayMenu; // 左右键都弹此菜单(纯原生跟踪,无动作派发)

        free((void *)icon);
        free((void *)tooltip);
    });
}

// zdTrayShutdownC 主线程同步移除状态栏项:调用方为退出流程的独立 goroutine,
// 此时主循环仍在运行,dispatch_sync 会立即被执行——托盘图标与主窗同时消失,
// 不留"后台收尾期间图标还挂着"的残影(Windows 同款时序)。主循环若已停,
// 调用方不会走到这里(先于 desk.Quit 调用)。
void zdTrayShutdownC(void) {
    dispatch_sync(dispatch_get_main_queue(), ^{
        if (zdStatusItem != nil) {
            [[NSStatusBar systemStatusBar] removeStatusItem:zdStatusItem];
            [zdStatusItem release];
            zdStatusItem = nil;
            zdTrayMenu = nil;
        }
    });
}
