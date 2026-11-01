//go:build windows

// 窗口进程的启动辅助与 Win32 互斥量(与 zdl_shell 各持副本;内容刻意精简,
// 无诊断端点——窗口进程排障走引擎侧 pprof)。

package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"zdownloader/internal/logbuf"
)

// 命名互斥量(单实例判定):-shell 常驻组件 / -ui 窗口进程各持一把。
// mutexExiting 与 shell 侧同名为跨进程契约:「栈退出进行中」标志锁,由
// zdl_shell 在托盘/悬浮窗「退出」时建立、引擎优雅收尾全程持有、退净后
// 释放(进程死亡时内核亦销毁);窗口进程启动链探测到它即等待退净后再自拉
// 全新栈(见 uiEnter)
const (
	mutexShell   = `Local\ZDownloader-shell`
	mutexUI      = `Local\ZDownloader-ui`
	mutexExiting = `Local\ZDownloader-exiting`
)

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

// mutexExists 判定命名互斥量是否已被持有(OpenMutex 成功=已存在)。
// 不创建:创建语义归持有方(常驻组件/窗口进程各自 acquireMutex)。
func mutexExists(name string) bool {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	const synchronize = 0x00100000
	h, _, _ := k32.NewProc("OpenMutexW").Call(synchronize, 0, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(name))))
	if h != 0 {
		k32.NewProc("CloseHandle").Call(h)
		return true
	}
	return false
}

var mutexHandle atomic.Uintptr

func acquireMutex(name string) bool {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	h, _, err := k32.NewProc("CreateMutexW").Call(0, 1, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(name))))
	const errAlreadyExists = 183
	if err.(syscall.Errno) == errAlreadyExists {
		return false
	}
	mutexHandle.Store(h)
	return true
}

func releaseMutex() {
	if h := mutexHandle.Load(); h != 0 {
		syscall.NewLazyDLL("kernel32.dll").NewProc("CloseHandle").Call(h)
		mutexHandle.Store(0)
	}
}

// spawnDetached 分离启动子进程。windowsgui 子进程本就无控制台,HideWindow
// 抑制启动瞬时窗;父子互不随退,生命周期由持有句柄的一方显式管理。
func spawnDetached(exe string, args []string, hide bool) (*os.Process, error) {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: hide}
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
