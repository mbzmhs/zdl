//go:build !windows && !darwin

// 非 Windows/macOS 平台的托盘占位实现:不阻塞、无图标。
package tray

// Run 在非 Windows 平台立即返回 nil。
func Run(iconPNG []byte, tooltip string, open, openSettings, exit func()) error {
	return nil
}

// Close 在非 Windows 平台为空操作。
func Close() {}

// Notify 在非 Windows 平台为空操作。
func Notify(title, text string) {}
