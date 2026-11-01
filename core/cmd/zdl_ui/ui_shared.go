package main

// 双栈 UI(Fyne/-tags wails)共用的启动前置:单实例仲裁、常驻组件保障、
// 引擎就绪等待。栈相关的差异只在"窗口怎么画",前置链路完全一致。

import (
	"fmt"
	"log"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"zdownloader/internal/conf"
	"zdownloader/internal/nativeui"
)

// sibling 同目录兄弟进程名(三件套按固定文件名互拉):Windows 分发带 .exe
// 后缀,其余平台原样。与 cmd/zdl_shell 侧同规则。
func sibling(exePath, name string) string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(filepath.Dir(exePath), name)
}

// uiOpts 窗口进程启动参数(双栈共用)
type uiOpts struct {
	exePath   string
	dataDir   string
	cfg       conf.Config
	settings  bool   // 启动即直达设置页(常驻组件 summonUI 的 -settings)
	bootstrap string // 文件关联入口:种子路径或磁链(由本进程编排落持久槽)
}

// uiEnter 应用入口编排链(zdl_ui 是三件套的入口 exe:双击/系统关联/托盘
// 召唤都汇聚于此)——按当前运行状况逐层保障,全程不经引擎进程:
//  0. 栈退出锁探测:zdl_shell 正在执行「退出」(建锁→引擎收尾全程持锁)→
//     本进程等待其退净后再继续(而非直接退出):退出窗口期携带的意图
//     (文件关联的种子/磁链)随自拉的全新栈正常落槽建任务,目标不丢;
//  1. 常驻组件在跑(托盘被杀/首次直启则拉起 zdl_shell);
//  2. 引擎就绪(不在跑则由常驻组件拉起,本进程只轮询等待,避免双拉竞态);
//  3. 关联目标落持久槽(槽在引擎侧,UI/窗口在不在线目标都不丢);
//  4. 单实例仲裁:已有窗口实例 → 转交锋(带 settings/bootstrap 意图)后退出;
//     否则取得窗口互斥量,调用方继续建窗(本进程成为窗口)。
//
// 返回 (client, true)=可以继续建窗;返回 (nil, false)=已转交,调用方静默退出。
func uiEnter(o uiOpts) (*nativeui.Client, bool) {
	// 退出窗口期:栈正在「退出」(shell 持退出锁、引擎收尾中),此时新拉的 UI
	// 已收不到 uiExit 帧(引擎 SSE 随关停而关),直接启动会指着死引擎成孤儿。
	// 但不直接退出——直接退出会丢掉本次携带的目标(文件关联的种子/磁链)。改为
	// 等待退出完成再走正常链路:锁与 shell 单实例锁同属 shell 进程,壳放下退出
	// 锁即 os.Exit(进程死亡时内核销毁两把锁),下方自拉据此拉起一个全新栈,
	// 本进程再按携带意图落槽建窗,目标不丢且不成孤儿。
	if mutexExists(mutexExiting) {
		log.Println("栈退出进行中,等待退净后继续(携带意图随新栈落槽)")
		// 阶段一:等壳放下退出锁(引擎退净)。上限略大于壳的 8s 引擎等待兜底,
		// 正常 ~2s 内即解除;超时(异常卡死)才退化为直接退出,避免永久阻塞。
		deadline := time.Now().Add(15 * time.Second)
		for mutexExists(mutexExiting) && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
		if mutexExists(mutexExiting) {
			log.Println("等待栈退出超时,本进程直接退出")
			return nil, false
		}
		// 阶段二:放锁后壳随即 os.Exit,但存在极短"锁已放、进程未死"空档;
		// 再等壳退净(≤5s),确保下方自拉的是全新栈,而非将死(不会重启引擎)
		// 的旧壳——否则下方跳过自拉,waitForEngine 会空等满 20s 后失败。
		for i := 0; i < 50 && mutexExists(mutexShell); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
	// 常驻组件不在(托盘被杀/首次直启)→ 拉起 zdl_shell:引擎的拉起与窗口的
	// 召唤都挂在常驻组件上,窗口进程自身可以随时退出
	if !mutexExists(mutexShell) {
		// 常驻组件与窗口进程同目录分发(sibling)
		p, err := spawnDetached(sibling(o.exePath, "zdl_shell"),
			[]string{"-data", o.dataDir}, false)
		if err != nil {
			log.Printf("拉起常驻组件失败(托盘将不可用): %v", err)
		} else {
			log.Printf("已拉起常驻组件(pid=%d)", p.Pid)
			go func() {
				st, _ := p.Wait()
				log.Printf("常驻组件退出(pid=%d exit=%d)", p.Pid, st.ExitCode())
			}()
		}
	}

	// 引擎就绪后才连:引擎由常驻组件负责拉起(架构单一职责,窗口进程不 spawn
	// 引擎,避免双拉竞态);窗口只轮询等就绪(引擎初始化/任务恢复需数秒)
	client, err := waitForEngine(o)
	if err != nil {
		log.Fatal(err)
	}

	// 关联目标落持久槽(磁链/种子路径);已驻留或即将成为的窗口都会在 SSE
	// 接通/获焦时取槽弹 BT 对话框。落槽放在单实例仲裁之前:已驻留场景下转交
	// 唤起后,驻留窗口取的就是本进程刚落的槽
	if o.bootstrap != "" {
		body := map[string]string{}
		if strings.HasPrefix(strings.ToLower(o.bootstrap), "magnet:") {
			body["magnet"] = o.bootstrap
		} else {
			body["torrentPath"] = o.bootstrap
		}
		if err := client.PostJSON("/api/ui/newtask", body); err != nil {
			log.Printf("引导目标落槽失败: %v", err)
		}
	}

	// 单实例:已有窗口进程在跑(哪怕隐藏驻留)→ 经引擎 uiShow 中继唤起它,
	// 本进程退出。带着本次启动的意图转交(settings/bootstrap):托盘「设置」
	// 会以 -settings 拉起本进程,丢弃标志会让驻留窗口只到主界面
	if mutexExists(mutexUI) {
		if _, err := client.SummonUI(o.settings); err != nil {
			log.Printf("唤起已驻留窗口进程失败: %v", err)
		}
		log.Println("窗口进程已在运行,已唤起其窗口,本进程退出")
		return nil, false
	}
	if !acquireMutex(mutexUI) {
		log.Println("窗口进程已在运行,本进程退出")
		return nil, false
	}
	return client, true
}

// waitForEngine 轮询等引擎就绪(拉起职责归常驻组件):每轮重读 config.json,
// 引擎把实际绑定端口回写(8620 被占会顺延)后才可能 ping 通,据此重建客户端,
// 避免端口漂移后唤错。
func waitForEngine(o uiOpts) (*nativeui.Client, error) {
	cfgPath := filepath.Join(o.dataDir, "config.json")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		if c := conf.Load(cfgPath, ""); c.Port > 0 {
			cl := nativeui.New(fmt.Sprintf("http://127.0.0.1:%d", c.Port))
			if _, err := cl.Ping(); err == nil {
				return cl, nil
			}
		}
	}
	return nil, fmt.Errorf("引擎 20 秒内未就绪(常驻组件应已拉起 zdl.exe,见 zdownloader.log)")
}
