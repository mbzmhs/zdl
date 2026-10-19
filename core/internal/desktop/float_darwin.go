//go:build darwin

// macOS 悬浮下载窗:NSPanel + WKWebView 加载 /float.html(Wails v2 单主窗口
// 做不了第二窗,故 cgo 自建;与 Windows 的 Win32+GDI 自绘同一对外 API)。
// 数据流:页面自己轮询 /api/tasks(与主 UI 同源);可见性/位置沿用
// showFloatWindow 与 floatX/floatY/floatW/floatH/floatPosSet 配置字段——
// 注意 Mac 侧 floatX/floatY 语义为 Cocoa 全局坐标的「左上角」(y 自各屏
// 底部起算),与 Windows 的 y 向下坐标不同机不混存,无互迁移需求。
// 位置回存:windowDidMove → 导出桥 → PUT /api/config(与 Windows savePos 同 JSON)。
package desktop

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa -framework WebKit

#import "float_darwin.h"
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// floatWin 单实例(与 Windows 侧同款:包级指针供 //export 桥回调用)。
var (
	floatWinMu sync.Mutex
	floatWin   *FloatWindow
)

// FloatWindow 置顶悬浮进度小窗(真实现)。
type FloatWindow struct {
	baseURL    string
	onShow     func()
	onSettings func()
	onExit     func()

	client *http.Client
}

// NewFloatWindow 创建悬浮窗(不立即上屏;Start 后按 showFloatWindow 配置显隐)。
func NewFloatWindow(baseURL string, onShow, onSettings, onExit func()) *FloatWindow {
	f := &FloatWindow{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		onShow:     onShow,
		onSettings: onSettings,
		onExit:     onExit,
		client:     &http.Client{Timeout: 3 * time.Second},
	}
	floatWinMu.Lock()
	floatWin = f
	floatWinMu.Unlock()
	return f
}

// Start 启动配置轮询(1s):按 showFloatWindow 显隐,首次显示时带上记忆位置。
func (f *FloatWindow) Start() {
	go f.pollLoop()
}

// Close 收起悬浮窗(同步):退出流程调用,与主窗/托盘一起立即消失,
// 后台收尾期间不留残影。面板未创建时为空操作。
func (f *FloatWindow) Close() { C.zdFloatCloseC() }

// CloseFloatWindow 供退出流程调用的包级入口(悬浮窗可能尚未创建)。
func CloseFloatWindow() {
	floatWinMu.Lock()
	f := floatWin
	floatWinMu.Unlock()
	if f != nil {
		f.Close()
	}
}

// floatCfg 关心的配置子集。
type floatCfg struct {
	ShowFloatWindow bool `json:"showFloatWindow"`
	FloatX          int  `json:"floatX"`
	FloatY          int  `json:"floatY"`
	FloatW          int  `json:"floatW"`
	FloatH          int  `json:"floatH"`
	FloatPosSet     bool `json:"floatPosSet"`
}

func (f *FloatWindow) pollLoop() {
	var created, shown bool
	for {
		var cfg floatCfg
		if resp, err := f.client.Get(f.baseURL + "/api/config"); err == nil {
			err = json.NewDecoder(resp.Body).Decode(&cfg)
			resp.Body.Close()
			if err == nil {
				// 无进行中/做种任务时不显示(空速度+空列表的悬浮窗没有信息价值),
				// 有任务才出现、全部结束即隐藏;配置开关仍然优先
				show := cfg.ShowFloatWindow && f.hasActiveTask()
				switch {
				case show && !shown:
					if !created {
						// CString 所有权移交 zdFloatCreateC(异步块内使用,块尾释放;
						// 此处不得提前 free——块尚未执行,提前释放即 use-after-free)
						cURL := C.CString(f.baseURL + "/float.html")
						C.zdFloatCreateC(cURL, C.int(cfg.FloatX), C.int(cfg.FloatY),
							C.int(cfg.FloatW), C.int(cfg.FloatH), C.int(b2i(cfg.FloatPosSet)))
						created = true
					} else {
						C.zdFloatSetVisibleC(1)
					}
					shown = true
				case !show && shown:
					C.zdFloatSetVisibleC(0)
					shown = false
				}
			}
		}
		time.Sleep(time.Second)
	}
}

// hasActiveTask 是否有进行中/做种中的任务(决定悬浮窗显隐)。
// waiting/choosing/paused 等无传输状态的任务不计入。
func (f *FloatWindow) hasActiveTask() bool {
	resp, err := f.client.Get(f.baseURL + "/api/tasks")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var tasks []struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tasks); err != nil {
		return false
	}
	for _, t := range tasks {
		if t.Status == "running" || t.Status == "seeding" {
			return true
		}
	}
	return false
}

// savePos 位置尺寸回存(与 Windows savePos 同一 JSON 形状,配置字段跨平台共用)。
func (f *FloatWindow) savePos(x, y, w, h int) {
	body := fmt.Sprintf(`{"floatX":%d,"floatY":%d,"floatW":%d,"floatH":%d,"floatPosSet":true}`,
		x, y, w, h)
	req, err := http.NewRequest(http.MethodPut, f.baseURL+"/api/config", strings.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := f.client.Do(req); err == nil {
		resp.Body.Close()
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- //export 桥(ObjC 主线程回调 → Go;均转独立 goroutine 不阻塞主循环) ----

//export zdFloatOnMove
func zdFloatOnMove(x, y, w, h C.int) {
	floatWinMu.Lock()
	f := floatWin
	floatWinMu.Unlock()
	if f != nil {
		go f.savePos(int(x), int(y), int(w), int(h))
	}
}

//export zdFloatOnOpen
func zdFloatOnOpen() {
	floatWinMu.Lock()
	f := floatWin
	floatWinMu.Unlock()
	if f != nil && f.onShow != nil {
		go f.onShow()
	}
}

//export zdFloatMenuOpen
func zdFloatMenuOpen() {
	floatWinMu.Lock()
	f := floatWin
	floatWinMu.Unlock()
	if f != nil && f.onShow != nil {
		go f.onShow()
	}
}

//export zdFloatMenuSettings
func zdFloatMenuSettings() {
	floatWinMu.Lock()
	f := floatWin
	floatWinMu.Unlock()
	if f != nil && f.onSettings != nil {
		go f.onSettings()
	}
}

//export zdFloatMenuExit
func zdFloatMenuExit() {
	floatWinMu.Lock()
	f := floatWin
	floatWinMu.Unlock()
	if f != nil && f.onExit != nil {
		go f.onExit()
	}
}
