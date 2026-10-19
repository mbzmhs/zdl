//go:build windows

// Package tray 用纯 Win32 syscall 实现系统托盘图标(零第三方依赖)。
// Run 阻塞在消息循环;Close 供其他 goroutine 请求退出。
package tray

import (
	"bytes"
	"errors"
	"image/png"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	modUser32   = syscall.NewLazyDLL("user32.dll")
	modShell32  = syscall.NewLazyDLL("shell32.dll")
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")
	modGdi32    = syscall.NewLazyDLL("gdi32.dll")

	procGetModuleHandleW    = modKernel32.NewProc("GetModuleHandleW")
	procRegisterClassExW    = modUser32.NewProc("RegisterClassExW")
	procCreateWindowExW     = modUser32.NewProc("CreateWindowExW")
	procDefWindowProcW      = modUser32.NewProc("DefWindowProcW")
	procGetMessageW         = modUser32.NewProc("GetMessageW")
	procTranslateMessage    = modUser32.NewProc("TranslateMessage")
	procDispatchMessageW    = modUser32.NewProc("DispatchMessageW")
	procPostQuitMessage     = modUser32.NewProc("PostQuitMessage")
	procDestroyWindow       = modUser32.NewProc("DestroyWindow")
	procPostMessageW        = modUser32.NewProc("PostMessageW")
	procSetForegroundWindow = modUser32.NewProc("SetForegroundWindow")
	procGetCursorPos        = modUser32.NewProc("GetCursorPos")
	procCreatePopupMenu     = modUser32.NewProc("CreatePopupMenu")
	procAppendMenuW         = modUser32.NewProc("AppendMenuW")
	procTrackPopupMenu      = modUser32.NewProc("TrackPopupMenu")
	procCreateIconIndirect  = modUser32.NewProc("CreateIconIndirect")
	procCreateBitmap        = modGdi32.NewProc("CreateBitmap")
	procDeleteObject        = modGdi32.NewProc("DeleteObject")
	procShellNotifyIconW    = modShell32.NewProc("Shell_NotifyIconW")
)

const (
	wmAppTray       = 0x8000 // WM_APP + 0,托盘回调消息
	wmClose         = 0x0010
	wmNull          = 0x0000
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	nimAdd          = 0x00
	nimModify       = 0x01
	nimDelete       = 0x02
	nifMessage      = 0x01
	nifIcon         = 0x02
	nifTip          = 0x04
	nifInfo         = 0x10
	niifInfo        = 0x01
	tpmReturnCmd    = 0x0100
	tpmNonotify     = 0x0080
	tpmRightButton  = 0x0002
	mfString        = 0x0000
	mfSeparator     = 0x0800
	idmOpen         = 1
	idmSettings     = 2
	idmExit         = 3
)

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  uintptr
	LpszClassName uintptr
	HIconSm       uintptr
}

type msgW struct {
	HWnd     uintptr
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       pointW
	LPrivate uint32
}

type pointW struct{ X, Y int32 }

type notifyIconDataW struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UTimeout         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [4]uint32
	HBalloonIcon     uintptr
}

type iconInfoW struct {
	FIcon    uint32
	XHotspot uint32
	YHotspot uint32
	HbmMask  uintptr
	HbmColor uintptr
}

// 状态(单实例托盘)
var (
	stHwnd       uintptr
	stHIcon      uintptr
	stHMenu      uintptr
	stOpenFn     func()
	stSettingsFn func()
	stExitFn     func()
	stWndProc    uintptr
)

// Run 启动托盘图标并阻塞在消息循环。
// open 为左键/双击回调(打开界面);openSettings 为菜单"设置"回调;exit 为"退出"回调。
func Run(iconPNG []byte, tooltip string, open, openSettings, exit func()) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	stOpenFn, stSettingsFn, stExitFn = open, openSettings, exit

	hInst, _, _ := procGetModuleHandleW.Call(0)
	if hInst == 0 {
		return errors.New("GetModuleHandleW 失败")
	}

	clsName, _ := syscall.UTF16PtrFromString("ZDownloaderTrayWnd")
	stWndProc = syscall.NewCallback(wndProc)
	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   stWndProc,
		HInstance:     hInst,
		LpszClassName: uintptr(unsafe.Pointer(clsName)),
	}
	if r, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return errors.New("RegisterClassExW 失败")
	}

	title, _ := syscall.UTF16PtrFromString("ZDownloader")
	hwnd, _, _ := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(clsName)), uintptr(unsafe.Pointer(title)),
		0, 0, 0, 0, 0, 0, 0, hInst, 0)
	if hwnd == 0 {
		return errors.New("CreateWindowExW 失败")
	}
	stHwnd = hwnd

	icon, err := hiconFromPNG(iconPNG)
	if err != nil {
		return err
	}
	stHIcon = icon

	var nid notifyIconDataW
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = hwnd
	nid.UFlags = nifMessage | nifIcon | nifTip
	nid.UCallbackMessage = wmAppTray
	nid.HIcon = icon
	copyFixed(nid.SzTip[:], tooltip)
	if r, _, _ := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid))); r == 0 {
		return errors.New("添加托盘图标失败")
	}

	// 右键菜单
	hMenu, _, _ := procCreatePopupMenu.Call()
	openText, _ := syscall.UTF16PtrFromString("打开管理界面")
	settingsText, _ := syscall.UTF16PtrFromString("设置")
	exitText, _ := syscall.UTF16PtrFromString("退出")
	procAppendMenuW.Call(hMenu, mfString, idmOpen, uintptr(unsafe.Pointer(openText)))
	procAppendMenuW.Call(hMenu, mfString, idmSettings, uintptr(unsafe.Pointer(settingsText)))
	procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
	procAppendMenuW.Call(hMenu, mfString, idmExit, uintptr(unsafe.Pointer(exitText)))
	stHMenu = hMenu

	// 消息循环
	var m msgW
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || r == ^uintptr(0) { // WM_QUIT 或错误
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}

	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	procDeleteObject.Call(icon)
	stHwnd = 0
	return nil
}

// Close 请求关闭托盘与消息循环(可从任意 goroutine 调用)。
func Close() {
	if stHwnd != 0 {
		procPostMessageW.Call(stHwnd, wmClose, 0, 0)
	}
}

// Notify 弹出托盘气泡通知(Windows 10+ 呈现为操作中心的系统通知,即使
// 主窗口最小化/隐藏也可见)。托盘未运行(-no-tray / 尚未启动)时静默跳过;
// 可从任意 goroutine 调用(Shell_NotifyIcon 自身跨线程安全)。
func Notify(title, text string) {
	hwnd := stHwnd
	if hwnd == 0 {
		return
	}
	var nid notifyIconDataW
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = hwnd
	nid.UFlags = nifInfo
	copyFixed(nid.SzInfo[:], text)
	copyFixed(nid.SzInfoTitle[:], title)
	nid.DwInfoFlags = niifInfo
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

func wndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmAppTray:
		switch uint32(lParam) & 0xffff {
		case wmLButtonUp, wmLButtonDblClk:
			if stOpenFn != nil {
				stOpenFn()
			}
		case wmRButtonUp:
			showMenu(hwnd)
		}
		return 0
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case 0x0002: // WM_DESTROY
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return r
}

func showMenu(hwnd uintptr) {
	var pt pointW
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(hwnd)
	id, _, _ := procTrackPopupMenu.Call(
		stHMenu, tpmReturnCmd|tpmNonotify|tpmRightButton,
		uintptr(pt.X), uintptr(pt.Y), 0, hwnd, 0)
	procPostMessageW.Call(hwnd, wmNull, 0, 0)
	switch id {
	case idmOpen:
		if stOpenFn != nil {
			stOpenFn()
		}
	case idmSettings:
		if stSettingsFn != nil {
			stSettingsFn()
		}
	case idmExit:
		if stExitFn != nil {
			stExitFn()
		}
	}
}

// hiconFromPNG 解码 PNG 并构建 32bpp 预乘 alpha 的 HICON。
func hiconFromPNG(pngBytes []byte) (uintptr, error) {
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return 0, err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()

	// BGRA,行序自上而下。CreateBitmap 的 lpvBits 恒为顶行在前,并非 DIB 负高那种
	// "自下而上"约定;旧代码按自下而上填,被当顶行在前读,图标就被上下翻转(Z 显成反的)。
	bits := make([]byte, w*h*4)
	i := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			bits[i] = byte(bl >> 8)
			bits[i+1] = byte(g >> 8)
			bits[i+2] = byte(r >> 8)
			bits[i+3] = byte(a >> 8)
			i += 4
		}
	}
	hbmColor, _, errColor := procCreateBitmap.Call(
		uintptr(w), uintptr(h), 1, 32, uintptr(unsafe.Pointer(&bits[0])))
	if hbmColor == 0 {
		return 0, errors.New("CreateBitmap 失败: " + errStr(errColor))
	}
	maskBits := make([]byte, (w*h+7)/8) // 全 0 掩码,由颜色位图的 alpha 负责透明
	hbmMask, _, errMask := procCreateBitmap.Call(
		uintptr(w), uintptr(h), 1, 1, uintptr(unsafe.Pointer(&maskBits[0])))
	if hbmMask == 0 {
		procDeleteObject.Call(hbmColor)
		return 0, errors.New("CreateBitmap(mask) 失败: " + errStr(errMask))
	}
	ii := iconInfoW{FIcon: 1, HbmMask: hbmMask, HbmColor: hbmColor}
	hIcon, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	procDeleteObject.Call(hbmColor)
	procDeleteObject.Call(hbmMask)
	if hIcon == 0 {
		return 0, errors.New("CreateIconIndirect 失败")
	}
	return hIcon, nil
}

// copyFixed 复制 UTF-16 字符串到定长缓冲(带 null 结尾,超长截断)。
func copyFixed(dst []uint16, s string) {
	u, err := syscall.UTF16FromString(s)
	if err != nil {
		u = []uint16{0}
	}
	if len(u) > len(dst) { // 保证末尾 null
		u = u[:len(dst)-1]
		u[len(u)-1] = 0
	}
	copy(dst, u)
}

func errStr(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}
