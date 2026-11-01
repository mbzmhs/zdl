//go:build windows

// Win32 命名互斥量(单实例判定)+ 进程派生(自 shell.go 拆出的平台件;
// darwin 对应物见 mutex_darwin.go,两侧函数面刻意同构)。
package main

import (
	"log"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const (
	mutexShell = `Local\ZDownloader-shell`
	mutexUI    = `Local\ZDownloader-ui`
	// mutexExiting 与 zdl_ui 侧同名为跨进程契约:「栈退出进行中」标志锁。
	// 退出收摊全程持有,窗口进程启动链探测到即直接退出(见 zdl_ui uiEnter)
	mutexExiting = `Local\ZDownloader-exiting`
)

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

var exitLockHandle atomic.Uintptr

// acquireExitLock 建立「栈退出进行中」全局锁(与 zdl_ui 启动链的跨进程
// 契约,见 mutexExiting)。失败只记日志不阻断退出流程——至多退化回
// "退出窗口期新拉起的 UI 成孤儿"的罕见情形
func acquireExitLock() {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	h, _, _ := k32.NewProc("CreateMutexW").Call(0, 1, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(mutexExiting))))
	if h == 0 {
		log.Println("退出锁建立失败:退出窗口期新拉起的 UI 可能成孤儿")
		return
	}
	exitLockHandle.Store(h)
}

// releaseExitLock 释放退出锁(引擎退净后调用)。进程死亡时内核亦会销毁
// 该锁,显式释放仅作语义收束
func releaseExitLock() {
	if h := exitLockHandle.Swap(0); h != 0 {
		syscall.NewLazyDLL("kernel32.dll").NewProc("CloseHandle").Call(h)
	}
}

func spawnDetached(exe string, args []string, hide bool) (*os.Process, error) {
	cmd := exec.Command(exe, args...)
	// hide 置 STARTUPINFO 的 SW_HIDE = 子进程第一个窗口的初始显示状态:
	// Wails/WebView2 主窗吃该状态(hidden 派生则窗口生下来不可见),只对
	// 无窗进程(引擎)使用
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: hide}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}
