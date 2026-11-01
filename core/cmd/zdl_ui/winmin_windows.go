//go:build windows

// 最小化占位消息隔离——修复"副屏最小化再恢复,窗口被钳到最小尺寸"。
//
// 机理(读 fyne v2.8.2 glfw 驱动源码得出):Windows 最小化窗口时并不只是隐藏,
// 而是把窗口挪到 (-32000,-32000) 并缩成标题占位(约 160×28),随后照常发出
// WM_SIZE(SIZE_MINIMIZED)/WM_MOVE/WM_WINDOWPOSCHANGED。GLFW 把这些假尺寸/假坐标
// 原样报给 Fyne:processResized 把 w.width/height 污染成占位值;processMoved 发现
// "窗口落到主屏 DPI 区"→ detectScale 变化 → reloadScale → RescaleContext →
// fitContent——fitContent 读到被污染的 w.width/height(< 内容最小尺寸),置
// shouldWidth/Height=最小尺寸、shouldExpand=true;主循环见 shouldExpand 即
// view.SetSize(最小尺寸)。恢复时的真实尺寸事件虽会先到,但已排队的 expand 在其
// 之后执行,窗口最终被钳在最小尺寸。只有副屏复现的原因:最小化落点(-32000)按
// 主屏 DPI 计算,仅当窗口所在屏与主屏 DPI 不同才触发 reloadScale 这条链。
//
// 解法(不改 Fyne 源码,不维护 fork):子类化主窗窗口过程,把最小化过渡态的三条
// 占位消息吞掉——SIZE_MINIMIZED 尺寸、-32000 坐标的 WM_MOVE/WM_WINDOWPOSCHANGED。
// 三条各自独立判定(尺寸类型/坐标值),不依赖到达顺序;真实事件(恢复/最大化、
// 正常移动)原样转发。Fyne 看不到占位值,内部状态在恢复前后保持一致,天然无钳缩。
// 恢复/最大化/正常移动不受影响;Fyne 未注册 Iconify 回调,GLFW 内部对吞掉的消息
// 无依赖。子类化在首显拿到 HWND 后安装(C 侧幂等),CallWindowProcW 链回 GLFW 的
// 窗口过程;曾试过的子类化坑(二次安装自递归)由幂等守卫规避。
package main

/*
#include <windows.h>

static WNDPROC g_min_old_proc = NULL;

static LRESULT CALLBACK zdl_min_proc(HWND hwnd, UINT msg, WPARAM wParam, LPARAM lParam) {
    switch (msg) {
    case WM_SIZE:
        if (LOWORD(wParam) == SIZE_MINIMIZED) {
            return 0; // 吞:占位尺寸(约160×28)不得进入 Fyne
        }
        break;
    case WM_MOVE:
    case WM_WINDOWPOSCHANGED: {
        // -32000 是 Windows 最小化占位坐标(常量),按坐标判定与消息顺序无关
        int mx = 0, my = 0;
        if (msg == WM_MOVE) {
            mx = (short)LOWORD(lParam);
            my = (short)HIWORD(lParam);
        } else {
            WINDOWPOS *pwp = (WINDOWPOS *)lParam;
            if (pwp && !(pwp->flags & SWP_NOMOVE)) {
                mx = pwp->x;
                my = pwp->y;
            }
        }
        if (mx <= -31999 && my <= -31999) {
            return 0; // 吞:-32000 占位坐标不得进入 Fyne(防误判 DPI/记脏位置)
        }
        break;
    }
    }
    return CallWindowProcW(g_min_old_proc, hwnd, msg, wParam, lParam);
}

static void zdl_min_subclass(LONG_PTR hwnd) {
    if (g_min_old_proc) {
        return; // 幂等:重复子类化会把 g_min_old_proc 指向自己,消息递归栈溢出
    }
    g_min_old_proc = (WNDPROC)SetWindowLongPtrW((HWND)hwnd, GWLP_WNDPROC, (LONG_PTR)zdl_min_proc);
}

static void zdl_min_unsubclass(LONG_PTR hwnd) {
    if (g_min_old_proc) {
        SetWindowLongPtrW((HWND)hwnd, GWLP_WNDPROC, (LONG_PTR)g_min_old_proc);
        g_min_old_proc = NULL;
    }
}
*/
import "C"

// minSubclass 安装最小化消息隔离(首显拿到 HWND 后调用;C 侧幂等)。
// 句柄以整数穿越 cgo 边界:HWND 指针形式会触发 vet 的 unsafe.Pointer 误用
// 挡死构建(与 winpin 的 cgo 原因相同)。
func minSubclass(hwnd uintptr) {
	C.zdl_min_subclass(C.LONG_PTR(hwnd))
}

// minUnsubclass 退出前还原窗口过程
func minUnsubclass(hwnd uintptr) {
	C.zdl_min_unsubclass(C.LONG_PTR(hwnd))
}
