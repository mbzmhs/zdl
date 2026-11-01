//go:build windows

// libvlc 加载的 Windows 实现:LoadLibraryEx + LOAD_WITH_ALTERED_SEARCH_PATH。
// 为什么不用 syscall.NewLazyDLL:NewLazyDLL 走普通 LoadLibrary,不搜 DLL
// 自身目录——libvlc.dll 依赖同目录的 libvlccore.dll,从任意工作目录启动
// 都会因依赖解析失败报 126(仅当 libvlc 恰好在 PATH 或系统目录才侥幸可用)。
package vlc

import (
	"fmt"
	"syscall"
	"unsafe"
)

const loadWithAlteredSearchPath = 0x8

var kernel32 = syscall.NewLazyDLL("kernel32.dll")

// openLibvlc 加载指定路径的 libvlc.dll,返回模块句柄
func openLibvlc(path string) (uintptr, error) {
	p16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	h, _, callErr := kernel32.NewProc("LoadLibraryExW").Call(
		uintptr(unsafe.Pointer(p16)), 0, loadWithAlteredSearchPath)
	if h == 0 {
		return 0, fmt.Errorf("LoadLibraryEx(%s): %v", path, callErr)
	}
	return h, nil
}

// callVLC 按名调用 libvlc 导出函数(Windows:syscall.DLL 直调)。
// 错误码不检查:libvlc 查询类函数用返回值表达状态,设置类函数无返回;
// 调用面已按 spike 验证过的签名固定。
func callVLC(name string, args ...uintptr) (uintptr, uintptr) {
	d := &syscall.DLL{Name: "libvlc.dll", Handle: syscall.Handle(dllHandle)}
	r1, r2, _ := d.MustFindProc(name).Call(args...)
	return r1, r2
}
