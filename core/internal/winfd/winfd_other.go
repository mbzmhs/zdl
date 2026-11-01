//go:build !windows

// 非 Windows 占位:原生壳当前仅 Windows 交付;Darwin 接 NSOpenPanel、
// Linux 接 XDG Desktop Portal 时按同名 API 补齐。
// 调用方(nativeui/app.go)对 errUnsupported 一律回退 Fyne 内建对话框,
// 本占位不影响功能完整性;ErrCancelled 供调用方按平台无关语义吞掉。
package winfd

import "errors"

// Options 与 Windows 侧同构(占位)
type Options struct {
	Title            string
	OKLabel          string
	InitialDir       string
	FileName         string
	DefaultExtension string
	FilterName       string
	FilterSpec       string
	Owner            uintptr
}

// ErrCancelled 用户取消(与 Windows 侧 winfd.go 同名契约,标签互斥不冲突)
var ErrCancelled = errors.New("已取消")

var errUnsupported = errors.New("winfd 未支持当前平台")

func PickFolder(o Options) (string, bool, error) { return "", false, errUnsupported }
func PickFile(o Options) (string, bool, error)   { return "", false, errUnsupported }
func SaveFile(o Options) (string, bool, error)   { return "", false, errUnsupported }
