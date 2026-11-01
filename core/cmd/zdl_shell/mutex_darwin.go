//go:build darwin

// 锁文件互斥(单实例判定)+ 进程派生(Windows 命名互斥量的 darwin 对应物,
// 见 mutex_windows.go;函数面刻意同构)。flock(LOCK_EX|LOCK_NB) 由内核在
// 持有进程死亡时自动释放,与命名互斥量「进程死即销毁」语义精确对齐,崩溃/
// 被杀不留死锁。锁文件在用户临时目录(会话级、按用户隔离),与 cmd/zdl_ui
// 侧必须用同一套文件名——跨进程契约(mutexExiting 等待链),改名须两侧同步。
package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
)

// 锁文件(单实例判定):-shell 常驻组件 / -ui 窗口进程各持一把。
// exiting 与 zdl_ui 侧同名为跨进程契约:「栈退出进行中」标志锁,退出收摊
// 全程持有,窗口进程启动链探测到即等待退净(见 zdl_ui uiEnter)。
var (
	mutexShell   = lockPath("ZDownloader-shell.lock")
	mutexUI      = lockPath("ZDownloader-ui.lock")
	mutexExiting = lockPath("ZDownloader-exiting.lock")
)

// lockPath 锁文件统一落用户临时目录:会话级生命周期(重启自清,对应 Windows
// 命名互斥量的会话作用域)、按用户隔离($TMPDIR 为用户私有目录)。与
// cmd/zdl_ui 侧同规则,两侧字面量必须一致。
func lockPath(name string) string {
	return filepath.Join(os.TempDir(), name)
}

// mutexExists 判定锁是否已被持有:读持有者登记的 pid 并探测其存活。
// 不用「试探加锁」做探测——试探本身会瞬时持锁,两个并发探测者会互相误报
// 「已持有」(Windows 侧 OpenMutexW 是纯探测,无此语义,必须对齐;后果见
// zdl_ui 侧注释:双开竞态误判「栈退出进行中」空等 5s)。pid 死后文件残留:
// kill(pid,0) 探测即失效,下个持有者覆写;pid 复用误报窗口极小(tmpdir
// 会话级,重启自清)。
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

// 单实例锁 fd:持有期间保持打开(flock 随 fd 存活),releaseMutex 或进程
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

// 退出锁 fd:与单实例锁分属两把(shell 同时持有 mutexShell 与 mutexExiting)。
var exitLockFile atomic.Pointer[os.File]

// acquireExitLock 建立「栈退出进行中」全局锁(与 zdl_ui 启动链的跨进程
// 契约,见 mutexExiting)。失败只记日志不阻断退出流程——至多退化回
// "退出窗口期新拉起的 UI 成孤儿"的罕见情形
func acquireExitLock() {
	f, err := os.OpenFile(mutexExiting, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		log.Println("退出锁建立失败:退出窗口期新拉起的 UI 可能成孤儿")
		return
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return // 已有退出进行中(罕见):语义上等同已持锁
	}
	_, _ = f.Seek(0, 0)
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d", os.Getpid())
	exitLockFile.Store(f)
}

// releaseExitLock 释放退出锁(引擎退净后调用)。进程死亡时内核亦会释放
// flock,显式释放仅作语义收束
func releaseExitLock() {
	if f := exitLockFile.Swap(nil); f != nil {
		_ = f.Truncate(0)
		f.Close()
	}
}

// spawnDetached 分离启动子进程:新会话(Setsid)使子进程不随父退出,对齐
// Windows 侧「父子互不随退」;hide 在 darwin 无对应物(无 windowsgui 隐藏
// 启动窗问题),忽略即可。
func spawnDetached(exe string, args []string, hide bool) (*os.Process, error) {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}
