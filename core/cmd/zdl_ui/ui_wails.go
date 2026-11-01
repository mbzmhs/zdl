//go:build wails

// ZDownloader 桌面端窗口进程(zdl_ui,-tags wails 构建):Wails/WebView2 纯
// 窗口模式。与 ui.go(默认 Fyne 构建)互斥,二者实现同一入口 runUI。
//
// 与旧全家桶 Wails 壳的区别:无引擎、无服务端、无托盘——webview 经 Wails
// 资产服务器反向代理加载**引擎进程**服务的 Vue 管理界面(所有请求含 /api
// 与 SSE 都代理过去,Wails 对经资产管道的 HTML 注入运行时,故 window.go.main.App
// 桥可用);Go 侧只做窗口生命周期(单实例/召唤/uiExit 随行)与原生对话框桥。
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"zdownloader/internal/assoc"
	"zdownloader/internal/autostart"
)

func runUI(o uiOpts) {
	log.SetPrefix("[ui] ")

	client, ok := uiEnter(o)
	if !ok {
		return // 已唤起既有窗口实例,本进程静默退出
	}

	base := client.Base
	bridge := &App{base: base}

	// 引擎 uiShow/uiExit 帧的 Go 侧响应:召唤(SSE 中继到达)置前窗口;引擎
	// 退出则收摊(webview 里的 Vue 自行处理业务帧,这里只管窗口生命周期)
	go watchUIEvents(base, bridge)

	err := wails.Run(&options.App{
		Title:            "ZDownloader",
		Width:            1240,
		Height:           800,
		BackgroundColour: &options.RGBA{R: 0x0d, G: 0x11, B: 0x17, A: 255},
		// 全部请求反代到引擎:界面/静态资源/API/SSE 单一来源,web 产物更新
		// 不用重发本 exe(与 Fyne 模式的"引擎为数据面"同构)。FlushInterval
		// 立即冲刷,否则 SSE 流被缓冲、前端实时推送全堵住。
		AssetServer: &assetserver.Options{
			Handler: func() http.Handler {
				u, _ := url.Parse(base)
				rp := httputil.NewSingleHostReverseProxy(u)
				rp.FlushInterval = -1
				return rp
			}(),
		},
		OnStartup: func(ctx context.Context) { bridge.setCtx(ctx) },
		OnDomReady: func(ctx context.Context) {
			if !o.settings {
				return
			}
			// 常驻组件以 -settings 拉起 → 直达设置页。Vue 挂载晚于 DomReady,
			// 注入的 JS 自轮询等钩子就位(20×250ms),不用 Go 侧定时轰炸
			wruntime.WindowExecJS(ctx, `(function(){var n=0;(function p(){`+
				`if(window.__zdOpenSettings){window.__zdOpenSettings();return}`+
				`if(n++<20)setTimeout(p,250)})()})()`)
		},
		// 关窗语义(与 Fyne 版「关闭窗口时退出 UI 进程」开关同字段同语义):
		// 默认隐藏驻留(托盘唤起秒开);开关打开才真退。uiExit/托盘退出路径
		// 先置 quitting 放行,否则拦截会把主动退出也吞掉。
		OnBeforeClose: func(ctx context.Context) bool {
			if bridge.quitting.Load() {
				return false
			}
			if bridge.uiExitOnClose() {
				return false // 允许关闭 = UI 进程退出(内存归还 OS)
			}
			wruntime.WindowHide(ctx)
			return true // 拦下关闭 = 隐藏驻留
		},
		Bind: []interface{}{bridge},
		Windows: &windows.Options{
			// 用户数据目录放数据目录下:多实例互不锁 WebView2
			WebviewUserDataPath: filepath.Join(o.dataDir, "webview2"),
		},
	})
	if err != nil {
		log.Fatalf("Wails 窗口异常退出: %v", err)
	}
	log.Println("窗口进程已退出")
}

// watchUIEvents 订阅引擎 SSE(与 Vue 页内连接并行;窗口生命周期帧归 Go 侧)。
// 重连 100ms 起步、3s 封顶:断线盲区越短,托盘中继误判「无附着」的概率越低。
func watchUIEvents(base string, bridge *App) {
	backoff := 100 * time.Millisecond
	for {
		resp, err := http.Get(base + "/api/events?client=ui")
		if err == nil {
			scanUIEvents(resp, bridge)
			resp.Body.Close()
			backoff = 100 * time.Millisecond
		}
		time.Sleep(backoff)
		if backoff < 3*time.Second {
			backoff *= 2
		}
	}
}

// scanUIEvents 单条连接的行帧流消费(SSE 是行帧流,必须按行扫——
// json.Decoder 会被 "data: " 前缀噎住,连上即断、重连死循环——首个实测
// 版本就栽在这;": ping" 心跳行无 "data: " 前缀,天然被过滤)。
func scanUIEvents(resp *http.Response, bridge *App) {
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var msg struct {
			Type     string `json:"type"`
			Settings bool   `json:"settings"`
		}
		if json.Unmarshal([]byte(line[6:]), &msg) != nil {
			continue
		}
		switch msg.Type {
		case "uiShow":
			bridge.showWindow(msg.Settings)
		case "uiExit":
			bridge.quitting.Store(true) // 主动退出,放行 OnBeforeClose
			wruntime.Quit(bridge.ctx)
			return
		}
	}
}

// App 暴露给 webview JS 的原生能力桥。类型名即 JS 命名空间(Wails 按绑定
// 结构体的 Go 类型名生成 window.go.main.<TypeName>),改名会破坏前端契约。签名刻意
// 保持 (T, error) 两返回值——Wails 对该形态直接 resolve T,前端零适配。
type App struct {
	ctx      context.Context
	base     string
	quitting atomic.Bool
}

func (b *App) setCtx(ctx context.Context) { b.ctx = ctx }

// showWindow 召唤置前:恢复最小化 + 显示 + 置顶一次再还原(拖到最前)。
// macOS 后台应用的激活会被系统缓办(WindowShow 后窗口先落在当前前台应用
// 后面,数秒才浮上来,实测 15.x)——短暂置顶直接改写 Z 序把窗口拉到最前,
// 不依赖应用激活(1.8 全家桶壳同款);Windows 侧 WindowShow 即置前,不做
// 无谓闪动,按平台门控。settings 直达设置页:意图经 uiShow 帧到此,页面内
// 的 Vue 不知道召唤这回事,须由壳经 WindowExecJS 驱动其全局钩子。
func (b *App) showWindow(settings bool) {
	if b.ctx == nil {
		return
	}
	wruntime.WindowUnminimise(b.ctx)
	wruntime.WindowShow(b.ctx)
	if runtime.GOOS == "darwin" {
		wruntime.WindowSetAlwaysOnTop(b.ctx, true)
		time.Sleep(120 * time.Millisecond) // 置顶一拍:让窗级变更先于还原生效
		wruntime.WindowSetAlwaysOnTop(b.ctx, false)
	}
	// __zdWake=唤起后刷新快照并消费待弹创建窗;__zdOpenSettings=直达设置页
	// (Vue store 挂的全局钩子,webapp/src/store.js)
	js := `window.focus();if(window.__zdWake)window.__zdWake();`
	if settings {
		js += `if(window.__zdOpenSettings)window.__zdOpenSettings();`
	}
	wruntime.WindowExecJS(b.ctx, js)
}

// PickDirDialog 目录选择(返回 ""=取消)。浏览器安全模型拿不到文件系统绝对
// 路径,只有桌面壳能提供这个能力。
func (b *App) PickDirDialog(title string) (string, error) {
	if b.ctx == nil {
		return "", fmt.Errorf("窗口尚未就绪")
	}
	p, err := wruntime.OpenDirectoryDialog(b.ctx, wruntime.OpenDialogOptions{Title: title})
	if err != nil {
		return "", err
	}
	return p, nil // 取消时 wails 返回空串
}

// AutostartEnabled 开机自启实况(注册表 Run 键)
func (a *App) AutostartEnabled() (bool, error) {
	return autostart.Enabled(), nil
}

// SetAutostart 开关开机自启
func (a *App) SetAutostart(enabled bool) error {
	if enabled {
		return autostart.Enable()
	}
	return autostart.Disable()
}

// AssocStatus 文件关联实况
func (a *App) AssocStatus() (map[string]any, error) {
	return map[string]any{
		"platform": runtime.GOOS,
		"magnet":   assoc.MagnetEnabled(),
		"torrent":  assoc.TorrentEnabled(),
	}, nil
}

// SetAssoc 开关文件关联(写注册表;darwin 走接管流不经此)
func (a *App) SetAssoc(magnet, torrent bool) error {
	return assoc.Set(magnet, torrent)
}

// OpenAssocSystemSettings macOS:LaunchServices 接管(系统弹确认框)
func (a *App) OpenAssocSystemSettings() error {
	return assoc.OpenSystemSettings()
}

// uiExitOnClose 关窗语义开关:关窗瞬间回源引擎配置(设置页随时改,不能缓存)。
// 读失败按隐藏驻留(默认)处理——误退出比多驻留代价高。
func (b *App) uiExitOnClose() bool {
	cl := &http.Client{Timeout: 1200 * time.Millisecond}
	resp, err := cl.Get(b.base + "/api/config")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var cfg struct {
		UIExitOnClose bool `json:"uiExitOnClose"`
	}
	if json.NewDecoder(resp.Body).Decode(&cfg) != nil {
		return false
	}
	return cfg.UIExitOnClose
}

// SaveTorrentDialog 种子另存(前端取回字节后 base64 传入,壳弹原生另存对话框)
func (b *App) SaveTorrentDialog(dataB64, suggestedName string) (string, error) {
	if b.ctx == nil {
		return "", fmt.Errorf("窗口尚未就绪")
	}
	data, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return "", err
	}
	p, err := wruntime.SaveFileDialog(b.ctx, wruntime.SaveDialogOptions{
		Title:           "另存为",
		DefaultFilename: suggestedName,
	})
	if err != nil || p == "" {
		return "", err // p 为空 = 用户取消
	}
	if werr := os.WriteFile(p, data, 0o644); werr != nil {
		return "", werr
	}
	return p, nil
}
