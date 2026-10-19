//go:build windows

// 桌面版原生悬浮进度窗:置顶显示总速度与进行中任务的进度/速度。
// Wails v2 仅支持单主窗口,悬浮窗用 user32/gdi32 直接实现,运行在独立
// UI 线程(与托盘同模式)。支持拖动移动、拖边缩放,位置与尺寸写入配置记忆。
// 跨线程数据经锁传递,PostMessage 仅作唤醒,避免指针进消息被 GC 回收。
package desktop

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// ---------------- Win32 绑定 ----------------

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	dwmapi   = syscall.NewLazyDLL("dwmapi.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW") // 注意:在 kernel32 而非 user32

	procRegisterClassExW   = user32.NewProc("RegisterClassExW")
	procCreateWindowExW    = user32.NewProc("CreateWindowExW")
	procDefWindowProcW     = user32.NewProc("DefWindowProcW")
	procGetMessageW        = user32.NewProc("GetMessageW")
	procTranslateMessage   = user32.NewProc("TranslateMessage")
	procDispatchMessageW   = user32.NewProc("DispatchMessageW")
	procPostMessageW       = user32.NewProc("PostMessageW")
	procShowWindow         = user32.NewProc("ShowWindow")
	procSetWindowPos       = user32.NewProc("SetWindowPos")
	procBeginPaint         = user32.NewProc("BeginPaint")
	procEndPaint           = user32.NewProc("EndPaint")
	procFillRect           = user32.NewProc("FillRect")
	procDrawTextW          = user32.NewProc("DrawTextW")
	procInvalidateRect     = user32.NewProc("InvalidateRect")
	procLoadCursorW        = user32.NewProc("LoadCursorW")
	procGetClientRect      = user32.NewProc("GetClientRect")
	procGetCursorPos       = user32.NewProc("GetCursorPos")
	procCreatePopupMenu    = user32.NewProc("CreatePopupMenu")
	procAppendMenuW        = user32.NewProc("AppendMenuW")
	procTrackPopupMenu     = user32.NewProc("TrackPopupMenu")
	procDestroyMenu        = user32.NewProc("DestroyMenu")
	procGetSystemMetrics   = user32.NewProc("GetSystemMetrics")
	procGetWindowRect      = user32.NewProc("GetWindowRect")
	procSetProcessDPIAware = user32.NewProc("SetProcessDPIAware")
	procSystemPar          = user32.NewProc("SystemParametersInfoW")
	procIsWindowVisible    = user32.NewProc("IsWindowVisible")

	// 主窗任务栏隐藏:移出屏幕(-32000,WS_VISIBLE 保持、WebView2 不冻结)+ 扩展风格加
	// WS_EX_TOOLWINDOW、清 WS_EX_APPWINDOW(工具窗不进任务栏)。不最小化,故任务栏恢复等
	// 边界不会残留工具窗形态;普通最小化按钮始终可用。实测 WDA_HIDE 对本环境无效,弃用。
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procGetWindowLongW           = user32.NewProc("GetWindowLongW")
	procSetWindowLongW           = user32.NewProc("SetWindowLongW")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")

	procCreateSolidBrush      = gdi32.NewProc("CreateSolidBrush")
	procCreateFontW           = gdi32.NewProc("CreateFontW")
	procSelectObject          = gdi32.NewProc("SelectObject")
	procDeleteObject          = gdi32.NewProc("DeleteObject")
	procSetTextColor          = gdi32.NewProc("SetTextColor")
	procSetBkMode             = gdi32.NewProc("SetBkMode")
	procStretchDIBits         = gdi32.NewProc("StretchDIBits")
	procSetStretchBltMode     = gdi32.NewProc("SetStretchBltMode")
	procGetTextExtentPoint32W = gdi32.NewProc("GetTextExtentPoint32W")

	// 悬浮窗双缓冲:整帧画进内存位图后一次 BitBlt 上屏,消除逐笔直绘的中间帧闪烁
	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procBitBlt                 = gdi32.NewProc("BitBlt")
	procDeleteDC               = gdi32.NewProc("DeleteDC")

	procDwmSetWindowAttribute = dwmapi.NewProc("DwmSetWindowAttribute")
)

const (
	wsPopup      = 0x80000000
	wsThickFrame = 0x00040000 // 允许用户拖边调整悬浮窗大小
	wsExTopmost  = 0x00000008
	wsExTool     = 0x00000080
	wsExNoAct    = 0x08000000

	swHide = 0

	// 主窗扩展风格位:工具窗不进任务栏。WS_EX_TOOLWINDOW(0x80)已在上方 wsExTool 定义。
	wsExAppWindow = 0x40000 // WS_EX_APPWINDOW:让窗体现在于任务栏(撤工具窗时恢复)

	swpNoSize        = 0x0001
	swpNoMove        = 0x0002
	swpNoZOrder      = 0x0004
	swpNoActivate    = 0x0010
	swpShowWindow    = 0x0040
	swpNoOwnerZOrder = 0x0200

	wmDestroy       = 2
	wmMove          = 0x0003
	wmSize          = 0x0005
	wmNCCalcSize    = 0x0083
	wmNCHitTest     = 0x0084
	wmNCPaint       = 0x0085
	wmNCACTIVATE    = 0x0086
	wmPaint         = 0x000F
	wmEraseBkgnd    = 0x0014
	wmMouseMove     = 0x0200
	wmLButtonDown   = 0x0201
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205

	htClient        = 1
	htLeft          = 10
	htRight         = 11
	htTop           = 12
	htTopLeft       = 13
	htTopRight      = 14
	htBottom        = 15
	htBottomLeft    = 16
	htBottomRight   = 17
	wmEnterSizeMove = 0x0231
	wmExitSizeMove  = 0x0232

	dtSingleLine  = 0x0020
	dtVCenter     = 0x0004
	dtRight       = 0x0002
	dtEndEllipsis = 0x8000
	dtNoprefix    = 0x00000800

	idcArrow         = 32512
	fwNormal         = 400
	fwBold           = 700
	defaultCharset   = 1
	cleartypeQuality = 5
	transBkMode      = 1
	mfString         = 0x0000
	mfSeparator      = 0x0800
	tpmReturCmd      = 0x0100
	tpmRightButton   = 0x0002
	wmNull           = 0x0000

	spiGetWorkArea  = 0x0030
	dwmwaCornerPref = 33
	dwmcpRound      = 2

	halftoneStretch = 4 // SetStretchBltMode:邻域平均缩小(大图渲染→缩放抗锯齿)
	srccopy         = 0x00CC0020
	dibRGBColors    = 0

	wmAppFloatUpdate = 0x8001 // WM_APP+1:悬浮窗数据唤醒
)

// 运行期负值(int → uintptr 转换需符号扩展,不能写成常量转换)
var (
	hwndMessage = -3 // HWND_MESSAGE(保留)
	hwndTopmost = -1 // HWND_TOPMOST
	cchAll      = -1 // DrawTextW:以 NUL 结尾整串
	fontHeight  = -13
	gwLExStyle  = -20 // GetWindowLongW 的 GWL_EXSTYLE
)

// MSG 完整布局(x64,48 字节):GetMessageW 会写满整个结构,
// 少一个字段都是越界写(曾导致堆内存被 rcPaint 坐标覆盖)
type winMsg struct {
	hwnd    uintptr
	message uint32
	_       uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
	_       uint32
}

// PAINTSTRUCT 前 32 字节:hdc、fErase(+pad)、rcPaint
type psLayout struct {
	hdc     uintptr
	fErase  uint32
	_       uint32
	rcPaint rect
}

type rect struct {
	Left, Top, Right, Bottom int32
}

type wndClassEx struct {
	Size, Style            uint32
	WndProc                uintptr
	ClsExtra, WndExtra     int32
	Instance, Icon, Cursor uintptr
	Background             uintptr
	MenuName, ClassName    uintptr
	IconSm                 uintptr
}

func regCall(proc *syscall.LazyProc, args ...uintptr) uintptr {
	r, _, _ := proc.Call(args...)
	return r
}

func utf16Ptr(s string) uintptr {
	p, _ := syscall.UTF16PtrFromString(s)
	return uintptr(unsafe.Pointer(p))
}

// COLORREF 为 0x00BBGGRR
func setColor(r, g, b uint8) uintptr {
	return uintptr(uint32(b)<<16 | uint32(g)<<8 | uint32(r))
}

func roundCorners(hwnd uintptr) {
	pref := uint32(dwmcpRound)
	regCall(procDwmSetWindowAttribute, hwnd, dwmwaCornerPref, uintptr(unsafe.Pointer(&pref)), 4)
}

// mainHwnd 定位 Wails 主窗句柄。本进程顶层窗有三类:主窗、悬浮进度窗(工具窗,不可见/隐藏)、
// 以及若干 WebView2 内部辅助窗(非工具窗但不可见,不上屏)。按"本进程 + 非工具窗 + 可见"
// 判定,唯主窗三者皆中。不靠标题匹配——WebView2 加载后会把窗体标题改写为文档标题。
// 找不到返回 0。
func (a *App) mainHwnd() uintptr {
	ourPID := uint32(os.Getpid())
	var found uintptr
	cb := syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		var pid uint32
		procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if pid != ourPID {
			return 1 // 非本进程窗口
		}
		ex := regCall(procGetWindowLongW, hwnd, uintptr(gwLExStyle))
		if ex&wsExNoAct != 0 {
			return 1 // 悬浮进度窗(带 WS_EX_NOACTIVATE);主窗两者皆无
		}
		if regCall(procIsWindowVisible, hwnd) == 0 {
			return 1 // 不可见:WebView2 内部辅助窗(DWM 合成但不上屏)
		}
		found = hwnd // 主窗:本进程、无 NOACTIVATE、可见(工具窗隐藏态仍可见)
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return found
}

// savedRect 缓存主窗隐藏前的位置与尺寸,唤起时原样移回(仅一个主窗,包级即可)。
var savedRect struct{ l, t, w, h int32 }

// hiddenToTray 主窗处于「关闭到托盘」的离屏隐藏态。只有此态唤起才按 savedRect 移回;
// 正常可见时唤起绝不能动位置尺寸——savedRect 可能还是零值(会把窗口重置到 (0,0))。
var hiddenToTray bool

// winPosOnScreen 报告窗口矩形与虚拟桌面(所有显示器并集)是否有足够可见部分
// (可见宽≥100、高≥60,足够抓住标题栏),防止拔掉显示器后把窗口恢复到
// 不存在的屏幕上无法拖回。不满足时调用方应放弃记忆位置,回退居中默认。
func winPosOnScreen(x, y, w, h int) bool {
	const (
		smXVirtual  = 76
		smYVirtual  = 77
		smCXVirtual = 78
		smCYVirtual = 79
	)
	vx, vy := int(regCall(procGetSystemMetrics, smXVirtual)), int(regCall(procGetSystemMetrics, smYVirtual))
	vw, vh := int(regCall(procGetSystemMetrics, smCXVirtual)), int(regCall(procGetSystemMetrics, smCYVirtual))
	ix := max(min(x+w, vx+vw)-max(x, vx), 0)
	iy := max(min(y+h, vy+vh)-max(y, vy), 0)
	return ix >= 100 && iy >= 60
}

// hideToTray 关闭到托盘:记录窗口位置与尺寸后移出屏幕(-32000),再把扩展风格改为工具窗去
// 任务栏图标。窗口保持 WS_VISIBLE,WebView2 不挂起,唤起不冻结;因未最小化,从任务栏等路径
// 恢复不会残留工具窗形态,普通最小化按钮始终可用。
func (a *App) hideToTray() {
	h := a.mainHwnd()
	if h == 0 {
		return
	}
	var r [4]int32 // RECT{left, top, right, bottom}
	procGetWindowRect.Call(h, uintptr(unsafe.Pointer(&r[0])))
	savedRect.l, savedRect.t, savedRect.w, savedRect.h = r[0], r[1], r[2]-r[0], r[3]-r[1]
	regCall(procSetWindowPos, h, 0, ^uintptr(32000), ^uintptr(32000), uintptr(savedRect.w), uintptr(savedRect.h), swpNoZOrder|swpNoOwnerZOrder|swpNoActivate)
	ex := regCall(procGetWindowLongW, h, uintptr(gwLExStyle))
	regCall(procSetWindowLongW, h, uintptr(gwLExStyle), (ex|wsExTool)&^wsExAppWindow)
	a.offscreen = true // 离屏隐藏态:位置采样必须跳过(当前位置是屏外坐标)
	hiddenToTray = true
}

// showToScreen 从托盘唤起:离屏隐藏态先还原为普通窗扩展风格(最小化/关闭按钮恢复),
// 再把窗口移回隐藏前的位置并激活;正常可见(从未离屏)只置前,不动位置与尺寸。
// 找不到句柄时(开机静默自启 StartHidden 的窗口从未上屏,枚举只认可见窗)退回 Wails
// 原生显示——它持有自身窗体,首次唤起走 hasBeenShown 完整显示流程,同样不冻结。
// 窗口隐藏期间保持 WS_VISIBLE,WebView2 不冻结,唤起即响应。
func (a *App) showToScreen() {
	a.offscreen = false
	h := a.mainHwnd()
	if h == 0 {
		wruntime.WindowShow(a.wctx)
		return
	}
	if !hiddenToTray {
		regCall(procSetForegroundWindow, h)
		return
	}
	hiddenToTray = false
	ex := regCall(procGetWindowLongW, h, uintptr(gwLExStyle))
	regCall(procSetWindowLongW, h, uintptr(gwLExStyle), (ex&^wsExTool)|wsExAppWindow)
	regCall(procSetWindowPos, h, 0, uintptr(savedRect.l), uintptr(savedRect.t), uintptr(savedRect.w), uintptr(savedRect.h),
		swpNoZOrder|swpNoOwnerZOrder|swpNoActivate|swpShowWindow)
	regCall(procSetForegroundWindow, h)
}

// uiFonts 惰性创建并缓存三级字体(每 UI 线程一套,避免每次绘制泄漏 GDI 对象)
type uiFonts struct{ speed, name, small uintptr }

func (f *uiFonts) get() (speed, name, small uintptr) {
	if f.speed == 0 {
		f.speed = uiFont(fwBold, -15, "Segoe UI")             // 下载速度(头部上行,加粗)
		f.name = uiFont(fwNormal, -13, "Microsoft YaHei UI")  // 文件名
		f.small = uiFont(fwNormal, -12, "Microsoft YaHei UI") // 上传速度/大小等数据
	}
	return f.speed, f.name, f.small
}

func uiFont(weight, height int, family string) uintptr {
	h, _, _ := procCreateFontW.Call(
		uintptr(height), 0, 0, 0, uintptr(weight),
		0, 0, 0,
		defaultCharset, 0, 0, cleartypeQuality, 0,
		utf16Ptr(family))
	return h
}

// virtualScreen 返回全部显示器的包围矩形(多显示器/负坐标副屏均覆盖)
func virtualScreen() (x, y, w, h int32) {
	v, _, _ := procGetSystemMetrics.Call(76)
	x = int32(v)
	v, _, _ = procGetSystemMetrics.Call(77)
	y = int32(v)
	v, _, _ = procGetSystemMetrics.Call(78)
	w = int32(v)
	v, _, _ = procGetSystemMetrics.Call(79)
	h = int32(v)
	return
}

func workArea() rect {
	var rc rect
	regCall(procSystemPar, spiGetWorkArea, 0, uintptr(unsafe.Pointer(&rc)), 0)
	return rc
}

func apiGet(c *http.Client, url string) ([]byte, error) {
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// ---------------- 悬浮进度窗 ----------------

type floatTask struct {
	name     string
	sizeText string // 已下载 / 总大小(HLS 为 分片 x/y)
	pct      float64
	speedDl  int64 // 下载速度(做种行恒 0)
	speedUl  int64 // 上传速度(下载中 = 边下边传,做种 = 全量上传)
	showUp   bool  // BT 任务显示上传槽位(http/HLS 无上传语义)
}

// gdiSize GetTextExtentPoint32W 出参
type gdiSize struct{ cx, cy int32 }

type floatSnapshot struct {
	enabled      bool
	posSet       bool
	px, py       int
	fw, fh       int // 记忆的窗口尺寸(0=未手动调整)
	totalSpeed   int64
	totalUpSpeed int64
	runningAll   int // 运行中任务总数(超过展示上限时用于 "+N" 提示)
	tasks        []floatTask
}

// FloatWindow 置顶小窗:总速度 + 进行中任务进度;设置 showFloatWindow 控制。
// 可拖动移动、拖边缩放,位置与尺寸持久化(配置 floatX/Y/W/H)。
type FloatWindow struct {
	baseURL    string
	onShow     func() // 打开管理界面
	onSettings func() // 打开设置页
	onExit     func() // 退出程序

	mu     sync.Mutex
	hub    uintptr
	snap   floatSnapshot
	hist   []int64 // 最近 floatHistN 秒的总下载速度样本(mu 保护)
	histUp []int64 // 最近 floatHistN 秒的总上传速度样本(mu 保护;与 hist 同拍推入)

	// 以下字段仅悬浮窗 UI 线程读写
	hwnd       uintptr
	x, y       int32 // 当前位置(跟随用户拖动)
	w, h       int32 // 当前尺寸(初始自适应,可被用户调整)
	posSet     bool
	sizeSet    bool
	userSized  bool      // 用户手动调整过尺寸后,高度不再随行数自适应
	preW, preH int32     // 拖拽/缩放开始时的尺寸,用于判断是否真的缩放过
	dragActive bool      // 手动拖拽进行中(客户区左键按住)
	dragCX     int32     // 按下时光标 X
	dragCY     int32     // 按下时光标 Y
	dragWX     int32     // 按下时窗口左上 X
	dragWY     int32     // 按下时窗口左上 Y
	lastClick  time.Time // 上次左键按下时刻(time.Now;本窗不合成双击,自计时判双击)
	fonts      uiFonts
}

const (
	floatW      = 264 // 默认宽度
	floatHeader = 46  // 头部区高(速度文本 + 曲线;顶部留白与侧边 12px 对齐)
	floatChart  = 26  // 速度曲线区高度
	floatRow    = 44
	floatMaxRow = 4
	floatHistN  = 60 // 速度历史样本数(秒)

	wmClose = 0x0010
)

// CloseFloatWindow 退出流程调用:关闭悬浮窗,与托盘/主窗一起立即消失,
// 后台收尾期间不留残影。WM_CLOSE 投递到悬浮窗自己的 UI 线程销毁
// (跨线程不直接碰窗口句柄);hwnd 创建后不再变化,退出路径直接读取。
func CloseFloatWindow() {
	if f := floatWin; f != nil && f.hwnd != 0 {
		regCall(procPostMessageW, f.hwnd, wmClose, 0, 0)
	}
}

// quitAssist Windows 的 Quit(PostQuitMessage)立即生效,无需辅助。
func quitAssist() {}

// registerSystemShutdownObserver Windows 无 NSWorkspace 通知占位:系统关机经
// Win32 会话结束消息(WM_QUERYENDSESSION/WM_ENDSESSION)由 Wails 主循环原生处理。
func registerSystemShutdownObserver() {}

func NewFloatWindow(baseURL string, onShow, onSettings, onExit func()) *FloatWindow {
	return &FloatWindow{baseURL: baseURL, onShow: onShow, onSettings: onSettings, onExit: onExit}
}

func (f *FloatWindow) Start() {
	go f.uiLoop()
	go f.pollLoop()
}

func (f *FloatWindow) pollLoop() {
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		f.pollOnce(client)
		time.Sleep(time.Second)
	}
}

func (f *FloatWindow) pollOnce(client *http.Client) {
	snap := floatSnapshot{enabled: true}
	var cfg struct {
		ShowFloatWindow *bool `json:"showFloatWindow"`
		FloatX          *int  `json:"floatX"`
		FloatY          *int  `json:"floatY"`
		FloatW          *int  `json:"floatW"`
		FloatH          *int  `json:"floatH"`
		FloatPosSet     *bool `json:"floatPosSet"`
	}
	if b, err := apiGet(client, f.baseURL+"/api/config"); err == nil {
		_ = json.Unmarshal(b, &cfg)
		if cfg.ShowFloatWindow != nil {
			snap.enabled = *cfg.ShowFloatWindow
		}
		if cfg.FloatPosSet != nil && cfg.FloatX != nil && cfg.FloatY != nil {
			snap.posSet, snap.px, snap.py = *cfg.FloatPosSet, *cfg.FloatX, *cfg.FloatY
		}
		if cfg.FloatW != nil && cfg.FloatH != nil {
			snap.fw, snap.fh = *cfg.FloatW, *cfg.FloatH
		}
	}
	var stats struct {
		TotalSpeed   int64 `json:"totalSpeed"`
		TotalUpSpeed int64 `json:"totalUpSpeed"`
	}
	if b, err := apiGet(client, f.baseURL+"/api/stats"); err == nil {
		_ = json.Unmarshal(b, &stats)
		snap.totalSpeed = stats.TotalSpeed
		snap.totalUpSpeed = stats.TotalUpSpeed
	}
	var tasks []struct {
		Filename   string `json:"filename"`
		Status     string `json:"status"`
		Speed      int64  `json:"speed"`
		UpSpeed    int64  `json:"upSpeed"`
		Downloaded int64  `json:"downloaded"`
		Arrived    int64  `json:"arrived"`
		Size       int64  `json:"size"`
		SegDone    int    `json:"segDone"`
		SegTotal   int    `json:"segTotal"`
		Kind       string `json:"kind"`
	}
	if b, err := apiGet(client, f.baseURL+"/api/tasks"); err == nil {
		_ = json.Unmarshal(b, &tasks)
	}
	runningAll := 0
	for i := range tasks {
		t := &tasks[i]
		// 做种任务同样常驻悬浮窗(做种速度即上传速度,进度恒 100%):
		// 只收 running 会让纯做种场景行数为 0 → 整窗隐藏
		if t.Status != "running" && t.Status != "seeding" {
			continue
		}
		runningAll++
		pct := 0.0
		sizeText := ""
		if t.Kind == "hls" && t.SegTotal > 0 {
			pct = float64(t.SegDone) / float64(t.SegTotal) * 100
			sizeText = strconv.Itoa(t.SegDone) + "/" + strconv.Itoa(t.SegTotal) + " 分片"
		} else {
			// 进度/字节数用 arrived(落盘去重口径):downloaded 是含重下的
			// 累计接收流量,反复重连的任务会显示超过总大小,观感像坏了
			prog := t.Downloaded
			if t.Arrived > 0 {
				prog = t.Arrived
			}
			if t.Size > 0 {
				pct = float64(prog) / float64(t.Size) * 100
			}
			if prog > 0 || t.Size > 0 {
				sizeText = fmtBytes(prog) + " / " + fmtBytes(t.Size)
			}
		}
		name := t.Filename
		if name == "" {
			name = "未命名任务"
		}
		// 速度语义按状态拆分:运行行 = ↓speed + ↑upSpeed(边下边传);
		// 做种行 = ↑speed(speed 即上传速度,引擎口径),下行恒 0
		var dl, ul int64
		if t.Status == "seeding" {
			ul = t.Speed
		} else {
			dl = t.Speed
			ul = t.UpSpeed
		}
		snap.tasks = append(snap.tasks, floatTask{name: name, sizeText: sizeText, pct: pct,
			speedDl: dl, speedUl: ul, showUp: t.Kind == "bt"})
	}
	sort.Slice(snap.tasks, func(i, j int) bool {
		return snap.tasks[i].speedDl+snap.tasks[i].speedUl > snap.tasks[j].speedDl+snap.tasks[j].speedUl
	})
	if len(snap.tasks) > floatMaxRow {
		snap.tasks = snap.tasks[:floatMaxRow]
	}
	snap.runningAll = runningAll
	f.mu.Lock()
	f.hist = append(f.hist, snap.totalSpeed)
	if len(f.hist) > floatHistN {
		f.hist = f.hist[len(f.hist)-floatHistN:]
	}
	f.histUp = append(f.histUp, snap.totalUpSpeed)
	if len(f.histUp) > floatHistN {
		f.histUp = f.histUp[len(f.histUp)-floatHistN:]
	}
	f.snap = snap
	f.mu.Unlock()
	f.wake()
}

func (f *FloatWindow) wake() {
	if h := atomic.LoadUintptr(&f.hub); h != 0 {
		regCall(procPostMessageW, h, wmAppFloatUpdate, 0, 0)
	}
}

// floatState 记录悬浮窗句柄与高度(包级;仅悬浮窗 UI 线程写)
var floatState struct {
	hwnd uintptr
	h    int32
}

// floatWin 包级单例:WndProc 直接取用,避免 uintptr/unsafe.Pointer 往返
var floatWin *FloatWindow

func (f *FloatWindow) uiLoop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	regCall(procSetProcessDPIAware)

	hInst, _, _ := procGetModuleHandleW.Call(0)
	wc := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:   syscall.NewCallback(floatWndProc),
		Instance:  hInst,
		Cursor:    regCall(procLoadCursorW, 0, idcArrow),
		ClassName: utf16Ptr("ZDFloat"),
	}
	regCall(procRegisterClassExW, uintptr(unsafe.Pointer(&wc)))

	hwnd, _, _ := procCreateWindowExW.Call(
		wsExTopmost|wsExTool|wsExNoAct,
		utf16Ptr("ZDFloat"), utf16Ptr("ZDownloader 下载悬浮窗"),
		wsPopup|wsThickFrame,
		0, 0, floatW, floatHeader+floatRow,
		0, 0, hInst, 0)
	if hwnd == 0 {
		log.Printf("[float] 悬浮窗创建失败")
		return
	}
	floatWin = f
	f.w, f.h = floatW, floatHeader+floatRow
	atomic.StoreUintptr(&f.hub, hwnd)

	floatState.hwnd = hwnd
	floatState.h = floatHeader + floatRow

	roundCorners(hwnd)

	for {
		var m winMsg
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func floatWndProc(hwnd uintptr, m uint32, wp, lp uintptr) uintptr {
	f := floatWin
	switch m {
	case wmAppFloatUpdate:
		if f != nil {
			f.apply(hwnd)
		}
		return 0
	case wmPaint:
		drawFloat(f, hwnd)
		return 0
	case wmEraseBkgnd:
		// 双缓冲下 WM_PAINT 整帧覆盖客户区,背景擦除纯属多余且会先闪一下底色
		return 1
	case wmNCPaint:
		// 若本窗进入前景,系统即在 WM_NCPAINT 画出活动边框;不画,保持无边框。
		return 0
	case wmNCACTIVATE:
		// 永远拒绝激活:no-act 窗一旦被激活,WS_THICKFRAME 就画出活动边框
		// (右键唤起菜单时最易触发)。返回非 0 阻止激活 → 彻底无边框。
		return 1
	case wmLButtonDblClk:
		// 双击直接打开主界面(右键菜单里的"打开管理界面"同款行为)。
		// 主判定在 WM_LBUTTONDOWN 自计时;此处兜底(若系统仍合成双击消息)。
		if f != nil {
			f.lastClick = time.Time{}
			f.dragActive = false
			if f.onShow != nil {
				f.onShow()
			}
		}
		return 0
	case wmNCCalcSize:
		// 客户区占满整窗:去掉系统边框(白边),THICKFRAME 的缩放能力保留
		if wp != 0 {
			return 0
		}
	case wmNCHitTest:
		// 无边框后自算边缘命中,保留拖边缩放(6px 热区)
		mx := int32(int16(lp & 0xFFFF))
		my := int32(int16((lp >> 16) & 0xFFFF))
		var wr rect
		regCall(procGetWindowRect, hwnd, uintptr(unsafe.Pointer(&wr)))
		const m = 6
		onL, onR := mx < wr.Left+m, mx >= wr.Right-m
		onT, onB := my < wr.Top+m, my >= wr.Bottom-m
		switch {
		case onL && onT:
			return htTopLeft
		case onR && onT:
			return htTopRight
		case onL && onB:
			return htBottomLeft
		case onR && onB:
			return htBottomRight
		case onL:
			return htLeft
		case onR:
			return htRight
		case onT:
			return htTop
		case onB:
			return htBottom
		}
		return htClient
	case wmMove:
		// 记录拖动后的位置(lParam: LOWORD=x, HIWORD=y, 均为有符号 16 位)
		if f != nil {
			f.x = int32(int16(lp & 0xFFFF))
			f.y = int32(int16((lp >> 16) & 0xFFFF))
		}
		return 0
	case wmSize:
		// 记录客户区尺寸(用户拖边缩放后生效);过小则拉回下限
		if f != nil {
			nw := int32(int16(lp & 0xFFFF))
			nh := int32(int16((lp >> 16) & 0xFFFF))
			minH := int32(floatHeader + floatRow)
			if nw < 220 || nh < minH {
				if nw < 220 {
					nw = 220
				}
				if nh < minH {
					nh = minH
				}
				regCall(procSetWindowPos, hwnd, 0, 0, 0,
					uintptr(nw), uintptr(nh), swpNoMove|swpNoActivate|swpNoZOrder)
			}
			f.w, f.h = nw, nh
		}
		return 0
	case wmLButtonDown:
		// 本窗是 WS_EX_NOACTIVATE 工具窗,系统不会为它合成 WM_LBUTTONDBLCLK,
		// 故在 WM_LBUTTONDOWN 里自计时判双击:两次按下间隔 ≤ 500ms(Win 默认
		// 双击间隔)即双击 → 开主界面;否则开始拖拽(光标不动则无位移)。
		// 用 Go 单调钟而非 GetTickCount(在 kernel32 而非 user32,proc 解析
		// 曾致本线程 panic),彻底避开 Win32 计时 proc。
		if f == nil {
			return 0
		}
		now := time.Now()
		if !f.lastClick.IsZero() && now.Sub(f.lastClick) <= 500*time.Millisecond {
			f.lastClick = time.Time{}
			f.dragActive = false
			if f.onShow != nil {
				f.onShow()
			}
			return 0
		}
		f.lastClick = now
		var pt struct{ x, y int32 }
		regCall(procGetCursorPos, uintptr(unsafe.Pointer(&pt)))
		var wr rect
		regCall(procGetWindowRect, hwnd, uintptr(unsafe.Pointer(&wr)))
		f.dragActive = true
		f.dragCX, f.dragCY = pt.x, pt.y
		f.dragWX, f.dragWY = wr.Left, wr.Top
		return 0
	case wmMouseMove:
		if f != nil && f.dragActive {
			var pt struct{ x, y int32 }
			regCall(procGetCursorPos, uintptr(unsafe.Pointer(&pt)))
			nx := f.dragWX + (pt.x - f.dragCX)
			ny := f.dragWY + (pt.y - f.dragCY)
			// SWP_NOACTIVATE:拖动时绝不让窗口激活,否则画出活动边框
			regCall(procSetWindowPos, hwnd, 0, uintptr(nx), uintptr(ny), 0, 0,
				swpNoSize|swpNoActivate|swpNoZOrder)
			f.x, f.y = nx, ny
		}
		return 0
	case wmLButtonUp:
		if f != nil && f.dragActive {
			f.dragActive = false
			go f.savePos(f.x, f.y, f.w, f.h)
		}
		return 0
	case wmEnterSizeMove:
		if f != nil {
			f.preW, f.preH = f.w, f.h
		}
		return 0
	case wmExitSizeMove: // 拖动/缩放结束:位置与尺寸写回配置(记忆)
		if f != nil {
			if f.w != f.preW || f.h != f.preH {
				f.userSized = true
			}
			x, y, w, h := f.x, f.y, f.w, f.h
			go f.savePos(x, y, w, h)
		}
		return 0
	case wmRButtonUp:
		if f != nil {
			f.showMenu(hwnd)
		}
		return 0
	case wmDestroy:
		return 0
	}
	return regCall(procDefWindowProcW, hwnd, uintptr(m), wp, lp)
}

// showMenu 右键菜单(与托盘菜单一致):打开管理界面 / 设置 / 退出。
func (f *FloatWindow) showMenu(hwnd uintptr) {
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	t1, _ := syscall.UTF16PtrFromString("打开管理界面")
	t2, _ := syscall.UTF16PtrFromString("设置")
	t3, _ := syscall.UTF16PtrFromString("退出")
	regCall(procAppendMenuW, hMenu, mfString, 1, uintptr(unsafe.Pointer(t1)))
	regCall(procAppendMenuW, hMenu, mfString, 2, uintptr(unsafe.Pointer(t2)))
	regCall(procAppendMenuW, hMenu, mfSeparator, 0, 0)
	regCall(procAppendMenuW, hMenu, mfString, 3, uintptr(unsafe.Pointer(t3)))

	var pt struct{ x, y int32 }
	regCall(procGetCursorPos, uintptr(unsafe.Pointer(&pt)))
	// 不 SetForegroundWindow(hwnd):那会把 no-act 窗拉成活动态,WS_THICKFRAME
	// 随即画出活动边框(右键菜单时可见)。菜单用 TPM_RIGHTBUTTON,关闭本就不
	// 依赖窗口激活,无需置前。
	id, _, _ := procTrackPopupMenu.Call(hMenu, tpmReturCmd|tpmRightButton,
		uintptr(pt.x), uintptr(pt.y), 0, hwnd, 0)
	regCall(procPostMessageW, hwnd, wmNull, 0, 0)
	regCall(procDestroyMenu, hMenu)

	switch id {
	case 1:
		if f.onShow != nil {
			f.onShow()
		}
	case 2:
		if f.onSettings != nil {
			f.onSettings()
		}
	case 3:
		if f.onExit != nil {
			f.onExit()
		}
	}
}

func (f *FloatWindow) apply(hwnd uintptr) {
	f.mu.Lock()
	snap := f.snap
	f.mu.Unlock()

	if !snap.enabled || len(snap.tasks) == 0 {
		regCall(procShowWindow, hwnd, swHide)
		return
	}
	autoH := int32(floatHeader + floatRow*len(snap.tasks))
	minH := int32(floatHeader + floatRow) // 头部 + 至少一行
	if !f.sizeSet {
		if snap.fw > 100 && int32(snap.fh) >= minH {
			f.w, f.h = int32(snap.fw), int32(snap.fh)
			f.userSized = true // 恢复用户记忆的尺寸
		} else {
			f.w, f.h = floatW, autoH
		}
		f.sizeSet = true
	}
	if !f.posSet {
		if snap.posSet {
			f.x, f.y = int32(snap.px), int32(snap.py) // 恢复上次保存的位置
		} else {
			rc := workArea()
			f.x, f.y = rc.Right-floatW-8, rc.Top+8
		}
		// 保存的位置落在所有显示器之外(移除显示器等)则回默认位,防窗口失踪
		vx, vy, vw, vh := virtualScreen()
		if f.x < vx || f.y < vy || f.x+f.w > vx+vw || f.y+f.h > vy+vh {
			rc := workArea()
			f.x, f.y = rc.Right-floatW-8, rc.Top+8
		}
		f.posSet = true
	}
	vis := regCall(procIsWindowVisible, hwnd) != 0
	if !vis {
		regCall(procSetWindowPos, hwnd, uintptr(hwndTopmost),
			uintptr(int32(f.x)), uintptr(int32(f.y)), uintptr(int32(f.w)), uintptr(int32(f.h)),
			swpNoActivate|swpShowWindow)
	} else if !f.userSized {
		// 未手动调过尺寸:高度随任务行数自适应,位置尊重用户拖动
		regCall(procSetWindowPos, hwnd, 0, 0, 0, uintptr(int32(f.w)), uintptr(int32(autoH)),
			swpNoMove|swpNoActivate|swpNoZOrder)
		f.h = autoH
	}
	regCall(procInvalidateRect, hwnd, 0, 0)
}

func drawFloat(f *FloatWindow, hwnd uintptr) {
	if f == nil {
		return
	}
	var s psLayout
	regCall(procBeginPaint, hwnd, uintptr(unsafe.Pointer(&s)))
	if s.hdc == 0 {
		return
	}
	defer regCall(procEndPaint, hwnd, uintptr(unsafe.Pointer(&s)))

	var cr rect
	regCall(procGetClientRect, hwnd, uintptr(unsafe.Pointer(&cr)))
	cw, ch := cr.Right, cr.Bottom
	if cw == 0 || ch == 0 {
		return
	}

	// 双缓冲:整帧先画进内存位图,最后一次性 BitBlt 上屏。逐笔直绘时背景→
	// 文本→曲线分多步写屏,每秒整窗重绘,屏幕会短暂露出画了一半的中间帧
	// (闪烁);缓冲后屏幕只见完整帧。
	mem := regCall(procCreateCompatibleDC, s.hdc)
	if mem == 0 {
		return
	}
	defer regCall(procDeleteDC, mem)
	bmp := regCall(procCreateCompatibleBitmap, s.hdc, uintptr(cw), uintptr(ch))
	if bmp == 0 {
		return
	}
	defer regCall(procDeleteObject, bmp)
	oldBmp := regCall(procSelectObject, mem, bmp)
	defer regCall(procSelectObject, mem, oldBmp)
	hdc := mem

	f.mu.Lock()
	snap := f.snap
	f.mu.Unlock()

	bg := regCall(procCreateSolidBrush, setColor(0x1F, 0x22, 0x29))
	full := rect{0, 0, cw, ch}
	regCall(procFillRect, hdc, uintptr(unsafe.Pointer(&full)), bg)
	regCall(procDeleteObject, bg)

	speedDl, name, small := f.fonts.get()
	regCall(procSetBkMode, hdc, transBkMode)
	// 头部两行速度文本(下载蓝/上传绿,颜色与曲线对应)。曲线起点按固定
	// 预留宽计算:若按实际内容测宽,数字一跳文本宽就变,曲线左缘逐帧
	// 横移(用户可见)。预留宽取最宽速度形态 "1023.9 MB/s" 在下载粗体
	// 下的实测宽——fmtSpeed 任何输出都不会比这个形态更宽,上行小字号
	// 更不会,故单次测量对两行都成立。
	dlText, ulText := fmtSpeed(snap.totalSpeed), fmtSpeed(snap.totalUpSpeed)
	regCall(procSelectObject, hdc, speedDl)
	regCall(procSetTextColor, hdc, setColor(0x4F, 0x8C, 0xFF))
	textW := measureText(hdc, "1023.9 MB/s")
	regCall(procSelectObject, hdc, small)
	regCall(procSetTextColor, hdc, setColor(0x34, 0xD3, 0x99))
	chartX0 := 12 + textW + 10
	more := snap.runningAll - len(snap.tasks)
	x1 := int32(cw - 14)
	if more > 0 {
		x1 = cw - 58
	}
	if x1-chartX0 < 36 { // 极窄窗口:压缩文字区,保证曲线最小宽度
		chartX0 = x1 - 36
	}
	// 颜色紧贴各次绘制设置:中间的行绘制会改写文本色,靠前设色会让
	// 头部两行全渲染成上一次设置的颜色(实测:两行全绿)
	regCall(procSelectObject, hdc, speedDl)
	regCall(procSetTextColor, hdc, setColor(0x4F, 0x8C, 0xFF))
	dr := rect{12, 11, chartX0 - 4, 28} // 顶部留白 11px ≈ 侧边 12px
	drawSingle(hdc, dlText, &dr)
	regCall(procSelectObject, hdc, small)
	regCall(procSetTextColor, hdc, setColor(0x34, 0xD3, 0x99))
	ur := rect{12, 27, chartX0 - 4, 43}
	drawSingle(hdc, ulText, &ur)
	if more > 0 {
		regCall(procSetTextColor, hdc, setColor(0x8A, 0x93, 0xA6))
		mr := rect{cw - 52, 17, cw - 10, 41}
		drawSingle(hdc, "+"+strconv.Itoa(more), &mr)
	}

	track := regCall(procCreateSolidBrush, setColor(0x39, 0x3F, 0x4C))
	fill := regCall(procCreateSolidBrush, setColor(0x4F, 0x8C, 0xFF))
	regCall(procSelectObject, hdc, name)
	y := int32(floatHeader - 2)
	for _, t := range snap.tasks {
		if y+floatRow > ch {
			break // 用户压缩了高度:放不下的行不绘制
		}
		// 文件名占满整行(超长省略号)
		regCall(procSetTextColor, hdc, setColor(0xDC, 0xE1, 0xEB))
		nr := rect{12, y, cw - 12, y + 17}
		drawSingle(hdc, t.name, &nr)
		// 进度条占满整行
		barY := y + 21
		tr := rect{12, barY, cw - 12, barY + 5}
		regCall(procFillRect, hdc, uintptr(unsafe.Pointer(&tr)), track)
		bw := int32(float64(cw-24) * t.pct / 100)
		if bw < 1 && t.pct > 0 {
			bw = 1
		}
		if bw > 0 {
			fr := rect{12, barY, 12 + bw, barY + 5}
			regCall(procFillRect, hdc, uintptr(unsafe.Pointer(&fr)), fill)
		}
		// 信息行:大小靠左灰字(超长省略号);速度在右侧固定槽位内右对齐
		// ——下载槽恒贴右缘(与 http/HLS 单槽行的下载位置对齐,视觉统一),
		// BT 行的上传槽排在其左。槽位右缘按最宽速度形态预留,数字增减只
		// 在槽内向左伸展,位置不随采样跳动;箭头缀尾:右对齐下末字符恒在
		// 槽位右缘,数字再变箭头也不挪位
		regCall(procSelectObject, hdc, small)
		wSlot := measureText(hdc, "1023.9 MB/s ↓")
		if u := measureText(hdc, "1023.9 MB/s ↑"); u > wSlot {
			wSlot = u
		}
		dlRight := cw - 12
		upRight := dlRight
		if t.showUp {
			upRight = dlRight - wSlot - 6
		}
		zr := rect{12, barY + 6, upRight - wSlot - 8, y + floatRow}
		if zr.Right < 48 { // 极窄窗口:至少给大小留一截,防矩形宽度为负
			zr.Right = 48
		}
		regCall(procSetTextColor, hdc, setColor(0x8A, 0x93, 0xA6))
		drawSingle(hdc, t.sizeText, &zr)
		if t.showUp {
			regCall(procSetTextColor, hdc, setColor(0x34, 0xD3, 0x99))
			urr := rect{upRight - wSlot, barY + 6, upRight, y + floatRow}
			if urr.Left < 12 {
				urr.Left = 12
			}
			drawRight(hdc, fmtSpeed(t.speedUl)+" ↑", &urr)
		}
		regCall(procSetTextColor, hdc, setColor(0x4F, 0x8C, 0xFF))
		drr := rect{dlRight - wSlot, barY + 6, dlRight, y + floatRow}
		if drr.Left < 12 {
			drr.Left = 12
		}
		drawRight(hdc, fmtSpeed(t.speedDl)+" ↓", &drr)
		y += floatRow
	}
	regCall(procDeleteObject, track)
	regCall(procDeleteObject, fill)

	drawSpeedChart(f, hdc, chartX0, x1)

	// 整帧就绪,一次上屏
	regCall(procBitBlt, s.hdc, 0, 0, uintptr(cw), uintptr(ch), mem, 0, 0, srccopy)
}

// drawSpeedChart 在 [x0,x1] 区间绘制总速度历史折线(最近 floatHistN 秒),
// 下载(蓝)与上传(绿)同轴叠加,纵向按窗口内两者最大速度归一化;叠层
// 顺序与详情页速度分页一致:上传在下、下载在上。
// 渲染走纯软件光栅:填充在 Go 里逐像素做 alpha 合成(下载填充半透明,
// 下层上传曲线/填充透出,不再被实心盖死),折线最后画(两色曲线互不遮
// 挡);4 倍超采样缓冲 + HALFTONE 缩小贴回(邻域平均,消除锯齿)。曾用
// GDI 图案刷+ROP2 做抖动半透明,实测部分环境两步全落空(填充消失),弃用。
func drawSpeedChart(f *FloatWindow, hdc uintptr, x0, x1 int32) {
	f.mu.Lock()
	hist := make([]int64, len(f.hist))
	copy(hist, f.hist)
	histUp := make([]int64, len(f.histUp))
	copy(histUp, f.histUp)
	f.mu.Unlock()
	if len(hist) < 2 || x1-x0 < 20 {
		return
	}
	top, bottom := int32(20), int32(40) // 随头部整体下移,顶部留白对齐侧边
	const ss = 4                        // 超采样倍数
	w, h := x1-x0, bottom-top
	mw, mh := w*ss, h*ss

	maxV := int64(1)
	for _, v := range hist {
		if v > maxV {
			maxV = v
		}
	}
	for _, v := range histUp {
		if v > maxV {
			maxV = v
		}
	}

	baseline := (bottom - 1 - top) * ss
	usable := float64((bottom - top - 4) * ss)
	// 曲线逐列 y 坐标:关键帧落在放大坐标系,列间线性插值
	curveY := func(samples []int64) []int32 {
		ns := int32(len(samples))
		ys := make([]int32, ns)
		for i, v := range samples {
			ys[i] = baseline - int32(float64(v)/float64(maxV)*usable)
		}
		out := make([]int32, mw)
		j := int32(0)
		for x := int32(0); x < mw; x++ {
			for j < ns-2 && (j+1)*mw/(ns-1) <= x {
				j++
			}
			xa, xb := j*mw/(ns-1), (j+1)*mw/(ns-1)
			if xb <= xa {
				out[x] = ys[j]
				continue
			}
			frac := float64(x-xa) / float64(xb-xa)
			out[x] = int32(float64(ys[j])*(1-frac) + float64(ys[j+1])*frac)
		}
		return out
	}

	buf := make([]byte, mw*mh*4) // BGRA,BI_RGB 忽略 alpha 字节
	setPx := func(x, y int32, r, g, b byte) {
		i := (y*mw + x) * 4
		buf[i], buf[i+1], buf[i+2] = b, g, r
	}
	blendPx := func(x, y int32, r, g, b byte, a uint32) {
		i := (y*mw + x) * 4
		buf[i] = byte((uint32(b)*a + uint32(buf[i])*(255-a)) / 255)
		buf[i+1] = byte((uint32(g)*a + uint32(buf[i+1])*(255-a)) / 255)
		buf[i+2] = byte((uint32(r)*a + uint32(buf[i+2])*(255-a)) / 255)
	}
	for i := int32(0); i < mw*mh; i++ { // 底色(缩小混色时曲线边缘才能正确过渡)
		buf[i*4], buf[i*4+1], buf[i*4+2] = 0x29, 0x22, 0x1F
	}

	// 填充:上传实心(最底层),下载半透明覆盖(下层透出)
	upY, dlY := curveY(histUp), curveY(hist)
	for x := int32(0); x < mw; x++ {
		for y := upY[x]; y <= baseline; y++ {
			setPx(x, y, 0x2B, 0x47, 0x3A)
		}
	}
	for x := int32(0); x < mw; x++ {
		for y := dlY[x]; y <= baseline; y++ {
			blendPx(x, y, 0x2B, 0x3B, 0x55, 170)
		}
	}
	// 折线最后画,ss 像素方柱连成 4 倍宽线,缩小后仍约 1px
	drawLine := func(cy []int32, r, g, b byte) {
		for x := int32(0); x < mw; x++ {
			for dy := int32(-ss / 2); dy < ss-ss/2; dy++ {
				setPx(x, cy[x]+dy, r, g, b)
			}
		}
	}
	drawLine(upY, 0x34, 0xD3, 0x99)
	drawLine(dlY, 0x4F, 0x8C, 0xFF)

	var bmi bmiHeader
	bmi.Size, bmi.Width, bmi.Height, bmi.Planes, bmi.BitCount = 40, mw, -mh, 1, 32
	regCall(procSetStretchBltMode, hdc, halftoneStretch)
	regCall(procStretchDIBits, hdc,
		uintptr(x0), uintptr(top), uintptr(w), uintptr(h),
		0, 0, uintptr(mw), uintptr(mh),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&bmi)), dibRGBColors, srccopy)
}

// savePos 拖动/缩放结束后把位置与尺寸持久化到配置(走既有 HTTP 接口)。
func (f *FloatWindow) savePos(x, y, w, h int32) {
	client := &http.Client{Timeout: 2 * time.Second}
	body := `{"floatX":` + strconv.FormatInt(int64(x), 10) +
		`,"floatY":` + strconv.FormatInt(int64(y), 10) +
		`,"floatW":` + strconv.FormatInt(int64(w), 10) +
		`,"floatH":` + strconv.FormatInt(int64(h), 10) + `,"floatPosSet":true}`
	req, err := http.NewRequest(http.MethodPut, f.baseURL+"/api/config", strings.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + "B"
	}
	v := float64(n)
	units := []string{"KB", "MB", "GB", "TB"}
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	s := strconv.FormatFloat(v, 'f', 1, 64)
	if len(s) > 3 && s[len(s)-2:] == ".0" {
		s = s[:len(s)-2]
	}
	return s + units[i]
}

func fmtSpeed(bps int64) string {
	const unit = 1024
	if bps < unit {
		return strconv.FormatInt(bps, 10) + " B/s"
	}
	v := float64(bps)
	units := []string{"KB/s", "MB/s", "GB/s", "TB/s"}
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	s := strconv.FormatFloat(v, 'f', 1, 64)
	if len(s) > 3 && s[len(s)-2:] == ".0" {
		s = s[:len(s)-2]
	}
	return s + " " + units[i]
}

func drawSingle(hdc uintptr, text string, r *rect) {
	regCall(procDrawTextW, hdc, utf16Ptr(text), uintptr(cchAll),
		uintptr(unsafe.Pointer(r)), dtSingleLine|dtVCenter|dtEndEllipsis|dtNoprefix)
}

// drawRight 右对齐绘制(悬浮窗的大小/速度合并灰字)
func drawRight(hdc uintptr, text string, r *rect) {
	regCall(procDrawTextW, hdc, utf16Ptr(text), uintptr(cchAll),
		uintptr(unsafe.Pointer(r)), dtSingleLine|dtVCenter|dtEndEllipsis|dtNoprefix|dtRight)
}

// measureText 返回 hdc 当前选中字体下 text 的像素宽度
func measureText(hdc uintptr, text string) int32 {
	u16, _ := syscall.UTF16FromString(text)
	var sz gdiSize
	regCall(procGetTextExtentPoint32W, hdc,
		uintptr(unsafe.Pointer(&u16[0])), uintptr(len(u16)-1), uintptr(unsafe.Pointer(&sz)))
	return sz.cx
}

// bmiHeader BITMAPINFOHEADER(32bpp BI_RGB 自绘缓冲给 StretchDIBits 用);
// Height 传负值 = 自上而下行序
type bmiHeader struct {
	Size                         uint32
	Width, Height                int32
	Planes, BitCount             uint16
	Compression, SizeImage       uint32
	XPelsPerMeter, YPelsPerMeter int32
	ClrUsed, ClrImportant        uint32
}
