//go:build windows

// Package winfd Windows 原生文件/目录选择器:COM IFileOpenDialog /
// IFileSaveDialog 纯 Go 直调(零 cgo、零第三方),与 Wails 壳、资源管理器
// 使用同一个系统对话框。
//
// 线程契约:每个公共函数内部 runtime.LockOSThread + CoInitializeEx(STA)。
// goroutine 不保证固定线程,COM STA 绑定线程,必须整段锁住执行;Show 是
// 模态调用(内部自带消息泵),在工作线程上合法。Owner 传入主窗 HWND 可
// 挂模态归属,传 0 则独立弹出(扩展转交场景无窗口可挂)。
//
// vtable 序号以 mingw-w64 shobjidl.h 为准(与 Wine IDL 一致;注意中段是
// GetFileName=16、GetResult=20——IFileDialog 没有 GetFileNameLabel)。
// 全部 COM 对象句柄保持 unsafe.Pointer 类型:vet 的 unsafeptr 检查禁止
// uintptr→Pointer 往返转换,句柄用 uintptr 存储无法过 vet,溯源型指针可以。
package winfd

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// Options 选择器参数(零值可用)
type Options struct {
	Title            string
	OKLabel          string  // 确认键文案(空 = 默认)
	InitialDir       string  // 初始目录(解析失败自动忽略)
	FileName         string  // 预填文件名(保存对话框)
	DefaultExtension string  // 保存默认扩展名(如 ".torrent")
	FilterName       string  // 过滤器显示名(如 "种子文件")
	FilterSpec       string  // 过滤模式(如 "*.torrent")
	Owner            uintptr // 所有者 HWND(0 = 无)
}

// ErrCancelled 用户取消(调用方按需吞掉)
var ErrCancelled = errors.New("已取消")

const (
	clsidFileOpenDialog = "{DC1C5A9C-E88A-4DDE-A5A1-60F82A20AEF7}"
	iidFileOpenDialog   = "{D57C7288-D4AD-4768-BE02-9D969532D960}"
	clsidFileSaveDialog = "{C0B4E2F3-BA21-4773-8DBA-335EC946EB8B}"
	iidFileSaveDialog   = "{84BCCD23-5FDE-4CDB-AEA4-AF64B83D78AB}"
	iidShellItem        = "{43826D1E-E718-42EE-BC55-A1E261C37BFE}"
	sigdnFileSysPath    = 0x80058000
	fosOverwritePrompt  = 0x2
	fosPickFolders      = 0x20
	fosForceFileSystem  = 0x40
	// hrCancelled HRESULT 0x800704C7(ERROR_CANCELLED)的 int32 补码值——
	// 无类型常量直接写 0x800704C7 会溢出 int32,故用补码字面量
	hrCancelled = -2147023673
	// hrECancelled E_CANCELLED(0x80004004)的 int32 补码:部分系统/关闭方式
	// 的取消返回此码而非 ERROR_CANCELLED
	hrECancelled = -2147467260

	modeFile   = 0
	modeFolder = 1
	modeSave   = 2
)

var (
	ole32   = syscall.NewLazyDLL("ole32.dll")
	shell32 = syscall.NewLazyDLL("shell32.dll")

	procCoInit   = ole32.NewProc("CoInitializeEx")
	procCoUninit = ole32.NewProc("CoUninitialize")
	procCoCreate = ole32.NewProc("CoCreateInstance")
	procCoTask   = ole32.NewProc("CoTaskMemFree")
	procSHItem   = shell32.NewProc("SHCreateItemFromParsingName")
)

// vcall COM 虚表调用:obj(对象指针)→ vtable 第 idx 项 → 按标准 x64 调用
// 约定传参。槽位只解一层引用(内容即函数地址);早期草稿对 fn 再解一层,
// 把函数代码前 8 字节当地址跳转 → 野指针崩溃(PC 稳定复现)。指针全程
// 保持 unsafe.Pointer 以通过 vet(见包注释)。
func vcall(obj unsafe.Pointer, idx uintptr, args ...uintptr) (r1 uintptr) {
	fn := *(*uintptr)(unsafe.Add(*(*unsafe.Pointer)(obj), idx*unsafe.Sizeof(uintptr(0))))
	r1, _, _ = syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return r1
}

// guidFromText "{XXXXXXXX-XXXX-...}" 解析为 16 字节 GUID(内存布局:前 3
// 字段小端,与 Windows GUID 二进制形态一致)
func guidFromText(s string) (g [16]byte) {
	// GUID 字符串 5 组(8-4-4-4-12):Data1/2/3 走 a/b/c,Data4 的 8 字节
	// = 第 4 组 2 字节 + 第 5 组 6 字节,直接落 e[0..7]。
	// 注意不要引入第 4 个标量变量:会把 Data4 首字节吃掉导致 GUID 错位
	//(实测表现为 CoCreateInstance 返回 REGDB_E_CLASSNOTREG)。
	var a, b uint32
	var c uint16
	var e [8]byte
	fmt.Sscanf(s, "{%08X-%04X-%04X-%02X%02X-%02X%02X%02X%02X%02X%02X}",
		&a, &b, &c, &e[0], &e[1], &e[2], &e[3], &e[4], &e[5], &e[6], &e[7])
	g[0] = byte(a)
	g[1] = byte(a >> 8)
	g[2] = byte(a >> 16)
	g[3] = byte(a >> 24)
	g[4] = byte(b)
	g[5] = byte(b >> 8)
	g[6] = byte(c)
	g[7] = byte(c >> 8)
	copy(g[8:], e[:])
	return g
}

func utf16p(s string) uintptr {
	if s == "" {
		return 0
	}
	p, _ := syscall.UTF16PtrFromString(s)
	return uintptr(unsafe.Pointer(p))
}

// utf16ToString 读 CoTaskMem 分配的 NUL 结尾 UTF-16 字符串
func utf16ToString(p unsafe.Pointer) string {
	var s []uint16
	for i := uintptr(0); ; i++ {
		c := *(*uint16)(unsafe.Add(p, i*2))
		if c == 0 {
			break
		}
		s = append(s, c)
	}
	return syscall.UTF16ToString(s)
}

// pick 内核:modeFile=文件选择,modeFolder=目录选择,modeSave=保存
func pick(o Options, mode int) (string, bool, error) {
	// COM STA 绑定线程:整段锁定(Show 模态泵消息,见包注释)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if r, _, _ := procCoInit.Call(0, 0x2 /*COINIT_APARTMENTTHREADED*/); int32(r) >= 0 {
		defer procCoUninit.Call()
	}
	// RPC_E_CHANGED_MODE(线程已按其他模式初始化):实测可用,不 Uninit
	// (所有权归先到的初始化方)

	// CoCreateInstance(CLSID, nil, INPROC_SERVER, IID, &dlg)
	var clsid, iid [16]byte
	switch mode {
	case modeSave:
		clsid = guidFromText(clsidFileSaveDialog)
		iid = guidFromText(iidFileSaveDialog)
	default:
		clsid = guidFromText(clsidFileOpenDialog)
		iid = guidFromText(iidFileOpenDialog)
	}
	var dlg unsafe.Pointer
	rc, _, _ := procCoCreate.Call(
		uintptr(unsafe.Pointer(&clsid[0])), 0, 1, /*CLSCTX_INPROC_SERVER*/
		uintptr(unsafe.Pointer(&iid[0])), uintptr(unsafe.Pointer(&dlg)))
	if rc != 0 || dlg == nil {
		return "", false, fmt.Errorf("CoCreateInstance 失败: 0x%x", rc)
	}
	defer vcall(dlg, 2) // Release

	// GetOptions(10) → SetOptions(9):追加 FOS 标志
	var opts uintptr
	vcall(dlg, 10, uintptr(unsafe.Pointer(&opts)))
	switch mode {
	case modeFolder:
		opts |= fosPickFolders | fosForceFileSystem
	case modeSave:
		opts |= fosOverwritePrompt | fosForceFileSystem
	default:
		opts |= fosForceFileSystem
	}
	vcall(dlg, 9, opts)

	if o.Title != "" {
		vcall(dlg, 17, utf16p(o.Title)) // SetTitle
	}
	if o.OKLabel != "" {
		vcall(dlg, 18, utf16p(o.OKLabel)) // SetOkButtonLabel
	}
	if o.FileName != "" {
		vcall(dlg, 15, utf16p(o.FileName)) // SetFileName
	}
	if o.DefaultExtension != "" {
		vcall(dlg, 22, utf16p(o.DefaultExtension)) // SetDefaultExtension
	}
	if mode == modeFile && o.FilterName != "" && o.FilterSpec != "" {
		// COMDLG_FILTERSPEC {name, spec} 双指针数组;SetFileTypes(4)
		spec := []struct{ name, pat uintptr }{
			{utf16p(o.FilterName), utf16p(o.FilterSpec)},
			{utf16p("所有文件"), utf16p("*.*")},
		}
		vcall(dlg, 4, uintptr(len(spec)), uintptr(unsafe.Pointer(&spec[0])))
		vcall(dlg, 5, 1) // SetFileTypeIndex
	}
	if o.InitialDir != "" {
		// SHCreateItemFromParsingName → SetFolder(12);失败静默(路径不存在等)
		iidSI := guidFromText(iidShellItem)
		var si unsafe.Pointer
		if r, _, _ := procSHItem.Call(utf16p(o.InitialDir), 0,
			uintptr(unsafe.Pointer(&iidSI[0])), uintptr(unsafe.Pointer(&si))); r == 0 && si != nil {
			vcall(dlg, 12, uintptr(si))
			vcall(si, 2) // Release
		}
	}

	// Show(3, hwndOwner):模态,内部自带消息泵
	showR1 := vcall(dlg, 3, o.Owner)
	switch int32(showR1) {
	case hrCancelled, hrECancelled:
		// 取消按关闭方式可能返回 ERROR_CANCELLED 或 E_CANCELLED 两种码,
		// 只认前者会把用户取消当真故障 → 调用方误走 Fyne 兜底再弹一层
		return "", false, ErrCancelled
	}
	if int32(showR1) < 0 {
		return "", false, fmt.Errorf("Show 失败: 0x%x", int32(showR1))
	}

	// GetResult(20) 取 IShellItem → GetDisplayName(SIGDN_FILESYSPATH,序号 5)
	var item unsafe.Pointer
	vcall(dlg, 20, uintptr(unsafe.Pointer(&item)))
	if item == nil {
		// S_OK 但无结果:部分取消路径如此返回,按取消语义处理(非基础设施故障)
		return "", false, ErrCancelled
	}
	defer vcall(item, 2) // Release

	var pwsz unsafe.Pointer
	vcall(item, 5, sigdnFileSysPath, uintptr(unsafe.Pointer(&pwsz)))
	if pwsz == nil {
		return "", false, ErrCancelled // 无法取路径:同"未产生可用选择"
	}
	defer procCoTask.Call(uintptr(pwsz))
	path := utf16ToString(pwsz)
	if path == "" {
		return "", false, ErrCancelled
	}
	return path, true, nil
}

// PickFolder 目录选择(资源管理器同款"选择文件夹"对话框)
func PickFolder(o Options) (string, bool, error) { return pick(o, modeFolder) }

// PickFile 文件选择(.torrent 等过滤器经 FilterName/FilterSpec)
func PickFile(o Options) (string, bool, error) { return pick(o, modeFile) }

// SaveFile 另存对话框(覆盖确认由 FOS_OVERWRITEPROMPT 提供)
func SaveFile(o Options) (string, bool, error) { return pick(o, modeSave) }
