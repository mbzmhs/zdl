//go:build windows

// Package autostart 以零依赖方式管理「开机自动启动」:
// 在 HKCU\Software\Microsoft\Windows\CurrentVersion\Run 写入带引号的
// 程序绝对路径(附 -no-browser,登录后静默常驻托盘)。
// 使用 HKCU 无需管理员权限;注册表即唯一事实来源,不写入 config.json。
package autostart

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// 测试时可覆盖为临时键路径,避免污染真实 Run 键。
var (
	runSubKey = `Software\Microsoft\Windows\CurrentVersion\Run`
	valueName = "ZDownloader"
)

const (
	hkeyCurrentUser = 0x80000001 // HKEY_CURRENT_USER
	keyQueryValue   = 0x0001
	keySetValue     = 0x0002
	regSZ           = 1 // REG_SZ

	errSuccess      = 0 // ERROR_SUCCESS
	errFileNotFound = 2 // ERROR_FILE_NOT_FOUND
)

var (
	advapi32             = syscall.NewLazyDLL("advapi32.dll")
	procRegOpenKeyExW    = advapi32.NewProc("RegOpenKeyExW")
	procRegCreateKeyExW  = advapi32.NewProc("RegCreateKeyExW")
	procRegSetValueExW   = advapi32.NewProc("RegSetValueExW")
	procRegQueryValueExW = advapi32.NewProc("RegQueryValueExW")
	procRegDeleteValueW  = advapi32.NewProc("RegDeleteValueW")
	procRegDeleteTreeW   = advapi32.NewProc("RegDeleteTreeW")
	procRegCloseKey      = advapi32.NewProc("RegCloseKey") // 无 W 后缀:无字符串参数,不分 ANSI/Unicode
)

// regCall 调用 advapi32 并返回 LSTATUS(0 = ERROR_SUCCESS)。
func regCall(proc *syscall.LazyProc, args ...uintptr) uintptr {
	r, _, _ := proc.Call(args...)
	return r
}

func utf16Ptr(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }

func openKey(desiredAccess uint32) (syscall.Handle, uintptr) {
	var h syscall.Handle
	code := regCall(procRegOpenKeyExW,
		hkeyCurrentUser,
		uintptr(unsafe.Pointer(utf16Ptr(runSubKey))),
		0, // ulOptions
		uintptr(desiredAccess),
		uintptr(unsafe.Pointer(&h)))
	return h, code
}

// commandString 返回写入注册表的启动命令(登录后不开浏览器,静默进托盘)。
func commandString() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return `"` + exe + `" -no-browser`, nil
}

// Enabled 报告当前是否已开启开机自启。
func Enabled() bool {
	h, code := openKey(keyQueryValue)
	if code != errSuccess {
		return false
	}
	defer regCall(procRegCloseKey, uintptr(h))
	return regCall(procRegQueryValueExW,
		uintptr(h),
		uintptr(unsafe.Pointer(utf16Ptr(valueName))),
		0, 0, 0, 0) == errSuccess
}

// currentRunValue 读取 Run 键里本程序当前登记的命令;值不存在返回 ok=false。
func currentRunValue() (string, bool) {
	h, code := openKey(keyQueryValue)
	if code != errSuccess {
		return "", false
	}
	defer regCall(procRegCloseKey, uintptr(h))
	var buf [1024]uint16
	size := uint32(len(buf) * 2)
	code = regCall(procRegQueryValueExW,
		uintptr(h),
		uintptr(unsafe.Pointer(utf16Ptr(valueName))),
		0, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)))
	if code != errSuccess || size < 2 {
		return "", false
	}
	return syscall.UTF16ToString(buf[:size/2]), true
}

// Enable 开机自启;已登记最新命令时跳过写值(幂等)。设置每次保存都会
// 携带全部开关,反复重写 Run 键值会触发杀软的自启动注册表防护误报(code 5)。
func Enable() error {
	cmd, err := commandString()
	if err != nil {
		return err
	}
	if cur, ok := currentRunValue(); ok && cur == cmd {
		return nil
	}
	var h syscall.Handle
	code := regCall(procRegCreateKeyExW,
		hkeyCurrentUser,
		uintptr(unsafe.Pointer(utf16Ptr(runSubKey))),
		0, // Reserved
		0, // lpClass
		0, // dwOptions: REG_OPTION_NON_VOLATILE
		uintptr(keySetValue),
		0, // lpSecurityAttributes
		uintptr(unsafe.Pointer(&h)),
		0) // lpdwDisposition
	if code != errSuccess {
		return fmt.Errorf("打开注册表 Run 键失败 (code %d)", code)
	}
	defer regCall(procRegCloseKey, uintptr(h))

	u16 := utf16Ptr(cmd)
	code = regCall(procRegSetValueExW,
		uintptr(h),
		uintptr(unsafe.Pointer(utf16Ptr(valueName))),
		0, // Reserved
		uintptr(regSZ),
		uintptr(unsafe.Pointer(u16)),
		uintptr(len(cmd)*2+2)) // 字节数,含 UTF-16 空终止符
	if code != errSuccess {
		return fmt.Errorf("写入注册表失败 (code %d)", code)
	}
	return nil
}

// Disable 关闭开机自启;值或键不存在视为已关闭。
func Disable() error {
	h, code := openKey(keySetValue)
	if code != errSuccess {
		if code == errFileNotFound {
			return nil
		}
		return fmt.Errorf("打开注册表 Run 键失败 (code %d)", code)
	}
	defer regCall(procRegCloseKey, uintptr(h))
	switch code := regCall(procRegDeleteValueW, uintptr(h),
		uintptr(unsafe.Pointer(utf16Ptr(valueName)))); code {
	case errSuccess, errFileNotFound:
		return nil
	default:
		return fmt.Errorf("删除注册表值失败 (code %d)", code)
	}
}

// RefreshSelf 在程序启动时调用:若已开启自启但注册表里的路径
// 与当前程序不一致(移动目录/升级覆盖),静默改写为新路径。
func RefreshSelf() {
	if !Enabled() {
		return
	}
	h, code := openKey(keyQueryValue)
	if code != errSuccess {
		return
	}
	var buf [1024]uint16
	size := uint32(len(buf) * 2)
	regCall(procRegQueryValueExW,
		uintptr(h),
		uintptr(unsafe.Pointer(utf16Ptr(valueName))),
		0, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)))
	regCall(procRegCloseKey, uintptr(h))

	want, err := commandString()
	if err != nil {
		return
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	if syscall.UTF16ToString(buf[:n]) != want {
		_ = Enable()
	}
}
