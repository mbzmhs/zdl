//go:build windows

// 窗口位置"钉位"——消除"启动先在居中/默认位、再跳到保存位"的闪帧。
//
// 背景:Fyne v2.8 的 Window 接口无 SetPosition/Move;首次 Show 内部是 create()(隐藏
// 建窗,位置取 CreateWindowEx 的 CW_USEDEFAULT,即 OS 默认的"居中"位)→ view.Show
// (SW_SHOW 上屏)。窗口以什么坐标"出生",就以什么坐标首帧上屏——公开 API 层没有
// 任何时机能在"出生前"改写它,这正是闪帧的根子。
//
// 曾试过两条路,均不通:
//  1. WNDPROC 子类化改写 WM_WINDOWPOSCHANGING:改写有效,但子类化要等 create() 产出
//     HWND,而"出生位置"在 create() 内部就定了——赶不上首次 Show(为此引入的"预创建
//     Show+Hide"只是把闪帧挪到预创建那次首显上,照闪,且子类化还引入二次安装自递归
//     栈溢出一类崩溃)。
//  2. WH_CALLWNDPROC 线程钩子:无需 HWND、能先于窗口过程看到 WM_WINDOWPOSCHANGING,
//     但实测改写不生效(WM_WINDOWPOSCHANGED 仍是原坐标;MSDN 亦明言该钩子只能查看、
//     不能修改消息)。
//
// 解法:WH_CBT 线程钩子的 HCBT_CREATEWND。窗口创建前钩子收到 CREATESTRUCT,MSDN
// 明确允许钩子过程修改其中的创建坐标——钉位激活期间把 CW_USEDEFAULT 的出生位改写为
// 保存位,窗口一"出生"就在保存位(仍隐藏);create() 后续不会移动它(应用不调
// CenterOnScreen,centered=false,fitContent 只改尺寸),view.Show 上屏即保存位,无帧
// 可闪。钩子只需线程 ID、无需 HWND,可在任何 Show 之前安装;复显(doShowAgain)走
// Fyne 自己跟踪的位置(=窗口最后所在位),本就无跳位,故预创建与子类化整体不需要。
// 改写仅限出生位为 CW_USEDEFAULT 的窗口:首显主窗即此情形;期间系统还会建 IME 等
// 隐藏辅助窗(显式坐标),与本问题无关,不碰。
//
// cgo 的原因:钩子过程需把 lParam 按 *CBT_CREATEWND/*CREATESTRUCT 解释,纯 Go 写法
// 会触发 go vet(unsafeptr) 的"possible misuse of unsafe.Pointer"挡死构建;指针解释
// 放 C 里天然干净。Fyne 本就依赖 MinGW cgo,不引入新工具链。
package main

/*
#include <windows.h>

// 钉位状态:C 侧全局,与 Go 侧 pinBegin/pinEnd 同步。钩子回调与 pinBegin/pinEnd
// 都在 Fyne 主线程执行(线程钩子于安装线程派发消息时回调),同线程无竞争。
static int   pin_enabled = 0;
static int   pin_x = 0;
static int   pin_y = 0;
static int   pin_w = 0; // 0 = 不钉尺寸(仅钉位置)
static int   pin_h = 0;
static HHOOK g_hook = NULL;

static LRESULT CALLBACK zdl_cbtproc(int nCode, WPARAM wParam, LPARAM lParam) {
    if (nCode == HCBT_CREATEWND && pin_enabled) {
        CBT_CREATEWND *cw = (CBT_CREATEWND *)lParam;
        if (cw && cw->lpcs &&
            cw->lpcs->x == CW_USEDEFAULT && cw->lpcs->y == CW_USEDEFAULT) {
            cw->lpcs->x = pin_x; // 改写"出生坐标":MSDN 明确允许钩子过程修改 CREATESTRUCT
            cw->lpcs->y = pin_y;
            // 出生尺寸同理:Fyne 按内容最小尺寸建窗,显示后再套保存尺寸会可见
            // 跳变(尺寸闪屏);出生即保存尺寸则首帧即终态
            if (pin_w > 0 && pin_h > 0) {
                cw->lpcs->cx = pin_w;
                cw->lpcs->cy = pin_h;
            }
        }
    }
    // 必须永远交还钩子链,绝不吞事件
    return CallNextHookEx(NULL, nCode, wParam, lParam);
}

// zdl_hook_install 在 Fyne 主线程调用:线程钩子按线程 ID 安装,无需窗口句柄,
// 可在任何 Show 之前装好。hMod 取本函数所在模块(即本 exe)的句柄。
void zdl_hook_install(void) {
    if (g_hook) {
        return;
    }
    HMODULE hMod = NULL;
    GetModuleHandleExW(GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS |
                           GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
                       (LPCWSTR)(const void *)&zdl_cbtproc, &hMod);
    g_hook = SetWindowsHookExW(WH_CBT, zdl_cbtproc, hMod, GetCurrentThreadId());
}

void zdl_hook_uninstall(void) {
    if (g_hook) {
        UnhookWindowsHookEx(g_hook);
        g_hook = NULL;
    }
}

void zdl_pin_begin(int x, int y, int w, int h) {
    pin_x = x; pin_y = y; pin_w = w; pin_h = h; pin_enabled = 1;
}
void zdl_pin_end(void)           { pin_enabled = 0; }
*/
import "C"

// pinHookInstall 在 Fyne 主线程装线程钩子(取当前线程 ID)。必须在任何 Show 之前
// 调用——fyne.Do 队列 FIFO,先于后续的 ShowUI 派发即满足。
func pinHookInstall() { C.zdl_hook_install() }

// pinHookUninstall 退出前卸钩子。
func pinHookUninstall() { C.zdl_hook_uninstall() }

// pinBegin 在触发 Show 前设定"出生矩形"(w/h≤0 时只钉位置)并激活:本次 Show
// 内建窗时 CW_USEDEFAULT 的出生位与 Fyne 默认出生尺寸都将被改写到此处。pinEnd
// 在 Show 后关闭。
func pinBegin(x, y, w, h int32) { C.zdl_pin_begin(C.int(x), C.int(y), C.int(w), C.int(h)) }

// pinEnd 关闭钉位(Show 完成后调用;此后建窗/移动不再被拦截)。
func pinEnd() { C.zdl_pin_end() }
