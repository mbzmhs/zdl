//go:build darwin

// 窗口进程的启动辅助与锁文件互斥(与 zdl_shell 各持副本;内容刻意精简,
// 无诊断端点——窗口进程排障走引擎侧 pprof)。
// Windows 的命名互斥量在 macOS 换成锁文件:flock(LOCK_EX|LOCK_NB) 由内核
// 在持有进程死亡时自动释放,与命名互斥量「进程死即销毁」语义精确对齐,
// 崩溃/被杀不留死锁。锁文件在用户临时目录(会话级、按用户隔离),shell 与
// ui 两侧必须用同一套文件名——这是两进程间的跨进程契约(见 uiEnter 的
// mutexExiting 等待链),改名须两侧同步。

package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"zdownloader/internal/logbuf"
)

// 锁文件(单实例判定):-shell 常驻组件 / -ui 窗口进程各持一把。
// exiting 与 shell 侧同名为跨进程契约:「栈退出进行中」标志锁,由
// zdl_shell 在托盘/悬浮窗「退出」时建立、引擎优雅收尾全程持有、退净后
// 释放(进程死亡时内核亦释放 flock);窗口进程启动链探测到它即等待退净后
// 再自拉全新栈(见 uiEnter)
var (
	mutexShell   = lockPath("ZDownloader-shell.lock")
	mutexUI      = lockPath("ZDownloader-ui.lock")
	mutexExiting = lockPath("ZDownloader-exiting.lock")
)

// lockPath 锁文件统一落用户临时目录:会话级生命周期(重启自清,对应 Windows
// 命名互斥量的会话作用域)、按用户隔离($TMPDIR 为用户私有目录)。与
// cmd/zdl_shell 侧同规则,两侧字面量必须一致。
func lockPath(name string) string {
	return filepath.Join(os.TempDir(), name)
}

// setupLogging 数据目录轮转日志 + 环形缓冲双写。三进程并发追加同一文件
// (O_APPEND 原子追加),各自 log.SetPrefix 区分来源。
func setupLogging(dataDir string) {
	logPath := filepath.Join(dataDir, "zdownloader.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	if st, e := f.Stat(); e == nil && st.Size() > 8<<20 {
		f.Close()
		_ = os.Rename(logPath, logPath+".old")
		f, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
	}
	log.SetOutput(io.MultiWriter(f, logbuf.Default()))
	log.SetFlags(log.LstdFlags)
}

// mutexExists 判定锁是否已被持有:读持有者登记的 pid 并探测其存活。
// 不用「试探加锁」做探测——试探本身会瞬时持锁,两个并发探测者会互相误报
// 「已持有」(实测:双开竞态下误判「栈退出进行中」,误入退出等待分支固定
// 空等 5s;Windows 侧 OpenMutexW 是纯探测,无此语义,必须对齐)。pid 死后
// 文件残留:kill(pid,0) 探测即失效,下个持有者覆写;pid 复用误报窗口极小
// (tmpdir 会话级,重启自清)。
func mutexExists(name string) bool {
	b, err := os.ReadFile(name)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err == nil || errors.Is(err, syscall.EPERM) {
		return true // EPERM=进程在但非本人所有,同视为存活
	}
	return false
}

// 锁 fd 登记:持有期间保持打开(flock 随 fd 存活),releaseMutex 或进程
// 退出即释放。
var mutexFile atomic.Pointer[os.File]

func acquireMutex(name string) bool {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return false
	}
	// 登记持有者 pid 供 mutexExists 纯探测(见其注释)
	_, _ = f.Seek(0, 0)
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d", os.Getpid())
	mutexFile.Store(f)
	return true
}

// releaseMutex 释放锁:清空 pid 文件再关 fd。残留的空文件无碍——持有语义
// 在 flock 与 pid 登记,探测对空文件恒 false。
func releaseMutex() {
	if f := mutexFile.Swap(nil); f != nil {
		_ = f.Truncate(0)
		f.Close()
	}
}

// spawnDetached 分离启动子进程:新会话(Setsid)使子进程不随父退出,对齐
// Windows 侧「父子互不随退」;hide 在 mac GUI 无意义(无 windowsgui 隐藏
// 启动窗问题),忽略即可。
func spawnDetached(exe string, args []string, hide bool) (*os.Process, error) {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}

// startDiagnostics 诊断端点(仅 127.0.0.1):pprof 四件套,窗口卡死时抓栈用
func startDiagnostics(port int) {
	mux := http.NewServeMux()
	mux.Handle("/debug/pprof/", pprof.Handler("index"))
	mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	mux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
	mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	srv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", port), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("诊断端点: http://127.0.0.1:%d/debug/pprof/", port)
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("诊断端点退出: %v", err)
		}
	}()
}
