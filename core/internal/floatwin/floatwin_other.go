//go:build !windows && !darwin

// 非 Windows 且非 macOS 暂无悬浮窗实现(Linux 可接 X11/Wayland 方案,
// 待常驻组件在 Linux 落地时接入):占位类型让常驻组件跨平台可编译,
// Start 为空操作。macOS 真实现见 floatwin_darwin.go(自 1.8 的
// internal/desktop 迁入)。
package floatwin

// FloatWindow 占位类型(Windows 侧为置顶自绘进度窗)。
type FloatWindow struct{}

// NewFloatWindow 创建悬浮窗(非 Windows 为空实现,不显示)。
func NewFloatWindow(baseURL string, onShow, onSettings, onExit func()) *FloatWindow {
	return &FloatWindow{}
}

// Start 启动数据轮询与 UI 线程(非 Windows 为空操作)。
func (f *FloatWindow) Start() {}

// CloseFloatWindow 退出流程调用(非 Windows 为空操作)。
func CloseFloatWindow() {}
