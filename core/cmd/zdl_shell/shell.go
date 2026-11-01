// 常驻组件(zdl_shell):托盘 + 悬浮窗。刻意不初始化 Fyne——本组件常驻不退出,
// 字体/GL 那套大头内存与它无关。职责(=旧守护拆出的「系统侧」半边):
//   - 保证引擎存活:不在则拉起 zdl,引擎退出(含托盘「退出」触发)则本组件
//     随之退出——组件退出即核心退出,不留孤儿托盘;
//   - 唤起窗口进程:zdl_ui 子进程存活 → 经引擎 uiShow 中继秒开(隐藏驻留);
//     已退出(用户开了「关窗退出 UI 进程」)→ 重新拉起;
//   - 悬浮进度窗:显隐随设置开关与在跑任务数(自轮询引擎,与托盘无交互);
//   - 托盘菜单明暗跟随主题:订阅引擎 SSE,消费 theme/uiExit 帧。
//
// 原生件均为跨平台同 API:托盘 internal/tray(darwin=NSStatusItem/win=Win32)、
// 悬浮窗 internal/floatwin(darwin=NSPanel+WKWebView 自 1.8 desktop 迁入,
// win=Win32 自绘);平台差异只剩进程互斥/派生,见 mutex_windows.go/mutex_darwin.go。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"zdownloader/internal/assets"
	"zdownloader/internal/conf"
	"zdownloader/internal/floatwin"
	"zdownloader/internal/nativeui"
	"zdownloader/internal/server"
	"zdownloader/internal/tray"
)

type shellOpts struct {
	exePath   string
	dataDir   string
	cfg       conf.Config
	portFlag  int
	pprofFlag int
}

// sibling 同目录兄弟进程名(三件套按固定文件名互拉):Windows 分发带 .exe
// 后缀,其余平台原样。与 cmd/zdl_ui 侧同规则。
func sibling(exePath, name string) string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(filepath.Dir(exePath), name)
}

func runShell(o shellOpts) {
	log.SetPrefix("[托盘] ")
	log.Printf("常驻组件启动: port=%d", o.cfg.Port)
	if !acquireMutex(mutexShell) {
		log.Println("常驻组件已在运行,本进程退出")
		return
	}
	defer releaseMutex()
	if o.pprofFlag > 0 {
		startDiagnostics(o.pprofFlag)
	}
	log.SetPrefix("[托盘] ") // startDiagnostics 不改前缀,防御性重设

	client := nativeui.New(fmt.Sprintf("http://127.0.0.1:%d", o.cfg.Port))

	// ---- 引擎保障:不在则拉起,退出则随行 ----
	engineProc, err := ensureEngineShell(o, client)
	if err != nil {
		log.Println(err)
		return
	}
	engineGone := make(chan struct{})
	go func() {
		if engineProc != nil {
			_, _ = engineProc.Wait() // 引擎退出(含 /api/shutdown)→ 组件随行
		} else {
			watchEngineDeath(client) // 附着的引擎非本组件所拉,轮询探活
		}
		close(engineGone)
		// 组件的生命周期锚定引擎(退出即核心退出,反之亦然):引擎没了,托盘
		// 必须跟着收(经 tray.Close 让消息循环返回),否则留下 summons 无门的
		// 孤儿托盘,且其互斥量会挡住下一个常驻组件
		log.Println("引擎已退出,常驻组件随行收摊")
		tray.Close()
	}()

	// ---- 窗口进程(zdlui Fyne)子进程跟踪与唤起 ----
	var childMu sync.Mutex
	var child *os.Process
	summonUI := func(settings, viaFrame bool) {
		// viaFrame=false(托盘点击):先走中继——窗口驻留(哪怕非本组件拉起/
		// 已隐藏)即经引擎 uiShow 帧秒开,零进程开销;引擎如实回报有无附着。
		// viaFrame=true(uiSpawn 帧):引擎已判定无附着,直接拉起,中继只会
		// 再触发一轮 uiSpawn 广播
		if !viaFrame {
			if ok, err := client.SummonUI(settings); err == nil && ok {
				return
			} else if err != nil {
				log.Printf("唤起窗口进程失败: %v", err)
			}
		}
		// 引擎判「无 UI 附着」≠ UI 进程不在:驻留窗口的 SSE 有断线自愈窗口
		// (重连节奏见 store sseLoop),此刻中继只是暂时无人接收。查 ui 锁文件
		// 分辨两种态:进程活着 → 等重连后重试中继,绝不双开(实测:SSE 断线
		// 窗口点托盘,双开一个 zdl_ui 再转交,唤窗延迟数秒);进程真不在 → spawn。
		// 重试仍失败也走 spawn:uiEnter 的 mutexUI 仲裁会让新进程转交即退出,
		// 僵死 UI 亦被兜住
		if mutexExists(mutexUI) {
			for i := 0; i < 3; i++ {
				time.Sleep(time.Second)
				if ok, err := client.SummonUI(settings); err == nil && ok {
					return
				}
			}
		}
		// 检查+spawn 原子化:托盘回调与 SSE uiSpawn watcher 可能同时到达——
		// 分离的 check/spawn 会双双通过 child==nil 检查,同秒拉起两个窗口
		// 进程(实测)。spawn 在锁内完成、登记 child 后再放锁。
		childMu.Lock()
		if child != nil {
			childMu.Unlock()
			return // 子进程在跑(暂态),不重复拉
		}
		args := []string{"-data", o.dataDir}
		if settings {
			args = append(args, "-settings")
		}
		// 窗口进程是可替换 UI 模块:按同目录固定名 summon,不感知其技术栈
		// (将来 Wails 等实现替换 zdl_ui 文件即完成切换)
		p, err := spawnDetached(sibling(o.exePath, "zdl_ui"), args, false)
		if err != nil {
			childMu.Unlock()
			log.Printf("拉起窗口进程失败: %v", err)
			return
		}
		child = p
		childMu.Unlock()
		go func() {
			_, _ = p.Wait() // 窗口进程退出(关窗退出开关/引擎消失)即回收
			childMu.Lock()
			if child == p {
				child = nil
			}
			childMu.Unlock()
			log.Printf("窗口进程已退出(pid=%d)", p.Pid)
		}()
		log.Printf("已拉起窗口进程(pid=%d)", p.Pid)
	}

	// 托盘菜单明暗初值=持久化主题;之后由 SSE theme 帧跟随 UI 上报
	tray.SetMenuTheme(o.cfg.Theme != "light")

	// ---- SSE 监听:uiSpawn(无 UI 附着时拉窗)/ theme(托盘明暗)
	//      / uiExit(引擎即将退出)----
	go watchEngineEvents(fmt.Sprintf("http://127.0.0.1:%d", o.cfg.Port),
		func(settings bool) { summonUI(settings, true) }, engineGone)

	// ---- 退出收摊:先把屏幕上看得见的东西收掉(悬浮窗、托盘图标),再通知
	//      引擎优雅退出——引擎收尾(任务停写/存盘)要数秒,若先等引擎,图标
	//      和悬浮窗会挂在桌面直到退净,观感像卡住。最后等引擎退净再随行进
	//      UI 子进程由 uiExit 帧自行收掉。托盘「退出」与悬浮窗右键菜单「退出」
	//      共用本函数。
	//      关键:回调在调用方的消息循环线程上执行(托盘线程/悬浮窗线程),而
	//      tray.Close() 只是向托盘线程自己的消息队列投 WM_CLOSE——就地阻塞等待
	//      的话该消息永远排不上处理,图标要等进程终结才消失,延迟修正完全失效。
	//      故整套收摊放到独立 goroutine,回调本身立即返回让消息循环继续转。
	var exitOnce sync.Once
	exitAll := func() {
		exitOnce.Do(func() {
			go func() {
				log.Println("退出:先收悬浮窗与托盘图标,再通知引擎优雅退出...")
				// 先立「栈退出进行中」全局锁:此后任何新拉起的 UI 进程
				// (本组件补拉/用户手拉/文件关联)启动链探测到即等待退净、
				// 再自拉全新栈(携带意图照常落槽),不成孤儿。锁须先于通知
				// 引擎建立——引擎关停一启动,新 UI 就可能收不到 uiExit 帧
				acquireExitLock()
				floatwin.CloseFloatWindow()
				tray.Close() // 托盘消息循环随即返回,图标立即消失
				if err := client.Shutdown(); err != nil {
					log.Printf("通知引擎退出失败: %v(引擎可能已退出)", err)
				}
				select {
				case <-engineGone: // 引擎退净再收组件
				case <-time.After(8 * time.Second):
				}
				// 引擎退净+悬浮窗已收:退出窗口期结束,放锁。此后新 UI
				// 启动走正常链路(shell 不在会自拉)
				releaseExitLock()
				log.Println("常驻组件退出")
				os.Exit(0)
			}()
		})
	}

	// ---- 悬浮进度窗:自轮询引擎,设置开关(showFloatWindow)开着且有运行中/
	//      做种任务时自动上屏,无任务自动隐藏;位置尺寸记忆在引擎配置。菜单
	//      动作接本组件的召唤/退出链(与托盘同款语义)。
	floatwin.NewFloatWindow(
		fmt.Sprintf("http://127.0.0.1:%d", o.cfg.Port),
		func() { summonUI(false, false) },
		func() { summonUI(true, false) },
		exitAll,
	).Start()

	// 托盘:左键唤窗/右键菜单(打开界面/设置/退出);退出=引擎优雅退出+组件随行
	if err := tray.Run(assets.TrayIcon,
		"ZDownloader v"+server.Version+" — 点击唤起界面",
		func() { summonUI(false, false) },
		func() { summonUI(true, false) },
		exitAll); err != nil {
		log.Printf("系统托盘不可用: %v", err)
	}
	// 托盘循环返回只有两种成因:①「退出」已请求——收摊 goroutine 在旁路线程
	// 通知引擎后自行 os.Exit;②引擎先退、随行 watcher 调 tray.Close
	// (此时 engineGone 已关)。两种情形 main 都不得抢在引擎优雅收尾前返回,
	// 否则本进程先死、引擎收不到关停指令成为孤儿——等待 engineGone 兜底
	<-engineGone
}

// ensureEngine 引擎不在则拉起 zdl.exe 并等就绪;在跑则返回 nil(附着模式)。
// 返回引擎进程句柄供退出随行(句柄归属:拉起模式才有,附着模式 nil 走轮询)。
func ensureEngineShell(o shellOpts, client *nativeui.Client) (*os.Process, error) {
	if _, err := client.Ping(); err == nil {
		log.Printf("附着已运行的引擎(127.0.0.1:%d)", o.cfg.Port)
		return nil, nil
	}
	args := []string{"-data", o.dataDir}
	if o.portFlag > 0 {
		args = append(args, "-port", fmt.Sprint(o.portFlag))
	}
	// 引擎与 zdlui 同目录分发(sibling),数据目录经 -data 显式传递
	p, err := spawnDetached(sibling(o.exePath, "zdl"), args, true)
	if err != nil {
		return nil, fmt.Errorf("拉起引擎失败: %w", err)
	}
	go func() { _, _ = p.Wait() }()
	log.Printf("引擎未运行,已拉起(pid=%d),等待就绪...", p.Pid)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if _, err := client.Ping(); err == nil {
			return p, nil
		}
	}
	return nil, fmt.Errorf("引擎 15 秒内未就绪(见 zdownloader.log)")
}

// watchEngineEvents 订阅引擎 SSE(?client=shell,不计入 UI 附着数):
// uiShow 帧 → 唤窗(关联启动的目标落槽时 UI 可能不在线,引擎广播唤起——
// 窗口进程的拉起权在本组件,必须由此响应,否则目标落槽却无人弹窗);
// theme 帧 → 托盘菜单明暗;uiExit 帧 → 引擎即将退出,组件随行。
func watchEngineEvents(base string, summon func(settings bool), engineGone chan struct{}) {
	cl := &http.Client{Timeout: 0}
	for {
		resp, err := cl.Get(base + "/api/events?client=shell")
		if err != nil {
			select {
			case <-engineGone:
				return
			case <-time.After(3 * time.Second):
				continue
			}
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var msg struct {
				Type     string `json:"type"`
				Theme    string `json:"theme"`
				Settings bool   `json:"settings"`
			}
			if json.Unmarshal([]byte(line[6:]), &msg) != nil {
				continue
			}
			switch msg.Type {
			case "uiSpawn":
				// 引擎已判定无 UI 附着:直接拉起窗口进程(不中继——中继调
				// /api/ui/show 会再广播,回环疯狂唤窗)
				summon(msg.Settings)
			case "theme":
				tray.SetMenuTheme(msg.Theme != "light")
			case "uiExit":
				return // 引擎退出流程已启动,组件随行(engineGone 兜底)
			}
		}
		resp.Body.Close()
		select {
		case <-engineGone:
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// watchEngineDeath 附着模式下轮询引擎探活(非本组件拉起,无进程句柄)
func watchEngineDeath(client *nativeui.Client) {
	for {
		time.Sleep(3 * time.Second)
		if _, err := client.Ping(); err != nil {
			return
		}
	}
}
