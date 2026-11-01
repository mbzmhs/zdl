//go:build !windows

// Unix 侧实现占位:macOS/Linux 走 purego dlopen(libvlc.dylib/.so),
// 与 Windows 侧共享 vlc.go 的 API 面。当前原生 UI 仅在 Windows 交付,
// Unix 构建保持可编译但 Load 恒失败——上层据此禁用播放入口即可。
package vlc

import "errors"

// openLibvlc purego dlopen 实现(待 Unix 交付时补:
//   - darwin: /Applications/VLC.app/Contents/MacOS/lib/libvlc.dylib,
//     或 bundle 内置 Frameworks
//   - linux: libvlc.so.5(常规发行版包 vlc))
func openLibvlc(path string) (uintptr, error) {
	return 0, errors.New("libvlc Unix 支持未实现")
}

// callVLC Unix 侧占位:交付时改为 purego RegisterLibFunc 的类型化函数表
func callVLC(name string, args ...uintptr) (uintptr, uintptr) {
	return 0, 0
}
