//go:build windows

// Package assoc 以零依赖方式管理「文件关联」:磁力链接协议(magnet:)与
// 种子文件(.torrent)的用户级注册表关联(HKCU\Software\Classes)。
// 注册表即唯一事实来源,不写入 config.json;HKCU 无需管理员权限。
// 关闭时只清理指向本程序的注册,不碰其他 BT 客户端的注册。
package assoc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// 测试可覆盖为沙箱键路径,避免污染真实注册表。
var (
	classesSub     = `Software\Classes`
	torrentUCSub   = `Software\Microsoft\Windows\CurrentVersion\Explorer\FileExts\.torrent\UserChoice`
	magnetUCSubFmt = `Software\Microsoft\Windows\Shell\Associations\UrlAssociations\magnet\UserChoice`
)

const (
	progIDTorrent = "ZDownloader.torrent"

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

// createKey 打开(或逐级创建)HKCU 下相对键,返回叶键句柄(KEY_SET_VALUE)。
func createKey(sub string) (syscall.Handle, error) {
	parts := strings.Split(sub, `\`)
	var h syscall.Handle
	for i := range parts {
		full := strings.Join(parts[:i+1], `\`)
		var k syscall.Handle
		code := regCall(procRegCreateKeyExW,
			hkeyCurrentUser,
			uintptr(unsafe.Pointer(utf16Ptr(full))),
			0, // Reserved
			0, // lpClass
			0, // dwOptions: REG_OPTION_NON_VOLATILE
			uintptr(keySetValue),
			0, // lpSecurityAttributes
			uintptr(unsafe.Pointer(&k)),
			0) // lpdwDisposition
		if code != errSuccess {
			if h != 0 {
				regCall(procRegCloseKey, uintptr(h))
			}
			return 0, fmt.Errorf("创建注册表键 %s 失败 (code %d)", full, code)
		}
		if h != 0 {
			regCall(procRegCloseKey, uintptr(h))
		}
		h = k
	}
	return h, nil
}

func openKeyQ(sub string) (syscall.Handle, uintptr) {
	var h syscall.Handle
	code := regCall(procRegOpenKeyExW,
		hkeyCurrentUser,
		uintptr(unsafe.Pointer(utf16Ptr(sub))),
		0, // ulOptions
		uintptr(keyQueryValue),
		uintptr(unsafe.Pointer(&h)))
	return h, code
}

// setSZ 写入 REG_SZ 值;name 为空串表示默认值。
func setSZ(h syscall.Handle, name, data string) error {
	u := utf16Ptr(data)
	code := regCall(procRegSetValueExW,
		uintptr(h),
		uintptr(unsafe.Pointer(utf16Ptr(name))),
		0, // Reserved
		uintptr(regSZ),
		uintptr(unsafe.Pointer(u)),
		uintptr(len(data)*2+2)) // 字节数,含 UTF-16 空终止符
	if code != errSuccess {
		return fmt.Errorf("写入注册表值 %s 失败 (code %d)", name, code)
	}
	return nil
}

// querySZ 读取 REG_SZ 值;不存在返回 (\"\", false)。
func querySZ(sub, name string) (string, bool) {
	h, code := openKeyQ(sub)
	if code != errSuccess {
		return "", false
	}
	defer regCall(procRegCloseKey, uintptr(h))
	var buf [1024]uint16
	size := uint32(len(buf) * 2)
	code = regCall(procRegQueryValueExW,
		uintptr(h),
		uintptr(unsafe.Pointer(utf16Ptr(name))),
		0, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)))
	if code != errSuccess || size < 2 {
		return "", false
	}
	return syscall.UTF16ToString(buf[:size/2]), true
}

// valueExists 报告值是否存在(空数据值也算存在)。
func valueExists(sub, name string) bool {
	h, code := openKeyQ(sub)
	if code != errSuccess {
		return false
	}
	defer regCall(procRegCloseKey, uintptr(h))
	return regCall(procRegQueryValueExW,
		uintptr(h),
		uintptr(unsafe.Pointer(utf16Ptr(name))),
		0, 0, 0, 0) == errSuccess
}

// deleteTree 删除键及其全部子键;键不存在视为已删除。
func deleteTree(sub string) error {
	code := regCall(procRegDeleteTreeW,
		hkeyCurrentUser,
		uintptr(unsafe.Pointer(utf16Ptr(sub))))
	if code != errSuccess && code != errFileNotFound {
		return fmt.Errorf("删除注册表键 %s 失败 (code %d)", sub, code)
	}
	return nil
}

// deleteDefaultValue 删除键的默认值;键或值不存在视为已删除。
func deleteDefaultValue(sub string) error {
	var h syscall.Handle
	code := regCall(procRegOpenKeyExW,
		hkeyCurrentUser,
		uintptr(unsafe.Pointer(utf16Ptr(sub))),
		0,
		uintptr(keySetValue),
		uintptr(unsafe.Pointer(&h)))
	if code != errSuccess {
		if code == errFileNotFound {
			return nil
		}
		return fmt.Errorf("打开注册表键 %s 失败 (code %d)", sub, code)
	}
	defer regCall(procRegCloseKey, uintptr(h))
	code = regCall(procRegDeleteValueW, uintptr(h), uintptr(unsafe.Pointer(utf16Ptr(""))))
	if code != errSuccess && code != errFileNotFound {
		return fmt.Errorf("删除默认值失败 (code %d)", code)
	}
	return nil
}

// launchCommand 关联启动命令(带引号的程序绝对路径 + "%1")。
func launchCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return `"` + exe + `" "%1"`, nil
}

// isOurCommand 判断注册表里的启动命令是否指向本程序:按 exe 文件名匹配,
// 程序移动目录后旧路径的注册仍认作自己的,可安全清理/刷新。
func isOurCommand(cmd string) bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(cmd), strings.ToLower(filepath.Base(exe)))
}

// MagnetEnabled 报告 magnet: 协议当前是否由本程序接管
// (命令指向本程序、URL Protocol 标记在位,且无其他程序的 UserChoice 覆盖)。
func MagnetEnabled() bool {
	cmd, ok := querySZ(classesSub+`\magnet\shell\open\command`, "")
	if !ok || !isOurCommand(cmd) {
		return false
	}
	if !valueExists(classesSub+`\magnet`, "URL Protocol") {
		return false
	}
	if uc, ok := querySZ(fmt.Sprintf(magnetUCSubFmt, ""), "ProgId"); ok {
		if uc != "" && uc != "magnet" {
			return false // 系统已为磁力链接指定其他默认程序
		}
	}
	return true
}

// TorrentEnabled 报告 .torrent 当前是否由本程序接管
// (.torrent 默认指向本程序 ProgID、命令指向本程序,且 UserChoice 无冲突)。
func TorrentEnabled() bool {
	if cur, ok := querySZ(classesSub+`\.torrent`, ""); !ok || cur != progIDTorrent {
		return false
	}
	cmd, ok := querySZ(classesSub+`\`+progIDTorrent+`\shell\open\command`, "")
	if !ok || !isOurCommand(cmd) {
		return false
	}
	if uc, ok := querySZ(torrentUCSub, "ProgId"); ok {
		if uc != "" && !strings.EqualFold(uc, progIDTorrent) {
			return false // 系统已为 .torrent 指定其他默认程序
		}
	}
	return true
}

// EnableMagnet 注册 magnet: 协议;已按当前命令注册完成时整段跳过
// (命令仍指向旧路径时照常重写,保留程序移动/升级覆盖后的自愈)。
// 设置每次保存都会携带全部开关,反复重写关联键会触发杀软的自启动
// 注册表防护误报(code 5)。
func EnableMagnet() error {
	cmd, err := launchCommand()
	if err != nil {
		return err
	}
	if cur, ok := querySZ(classesSub+`\magnet\shell\open\command`, ""); ok && cur == cmd &&
		valueExists(classesSub+`\magnet`, "URL Protocol") {
		return nil
	}
	h, err := createKey(classesSub + `\magnet`)
	if err != nil {
		return err
	}
	defer regCall(procRegCloseKey, uintptr(h))
	if err := setSZ(h, "", "URL:magnet 协议"); err != nil {
		return err
	}
	if err := setSZ(h, "URL Protocol", ""); err != nil {
		return err
	}
	hc, err := createKey(classesSub + `\magnet\shell\open\command`)
	if err != nil {
		return err
	}
	defer regCall(procRegCloseKey, uintptr(hc))
	return setSZ(hc, "", cmd)
}

// DisableMagnet 取消 magnet: 协议注册;仅当命令指向本程序时才删除,
// 其他程序的注册不动(视为已不在本程序名下)。
func DisableMagnet() error {
	if cmd, ok := querySZ(classesSub+`\magnet\shell\open\command`, ""); ok && !isOurCommand(cmd) {
		return nil
	}
	return deleteTree(classesSub + `\magnet`)
}

// EnableTorrent 注册 .torrent 打开方式:本程序 ProgID + 扩展名默认指向
// (用户级覆盖 HKCR,不动 UserChoice);已按当前命令注册完成时整段跳过
// (跳过理由同 EnableMagnet)。
func EnableTorrent() error {
	cmd, err := launchCommand()
	if err != nil {
		return err
	}
	if cur, ok := querySZ(classesSub+`\`+progIDTorrent+`\shell\open\command`, ""); ok && cur == cmd {
		if def, ok2 := querySZ(classesSub+`\.torrent`, ""); ok2 && def == progIDTorrent {
			return nil
		}
	}
	hp, err := createKey(classesSub + `\` + progIDTorrent)
	if err != nil {
		return err
	}
	defer regCall(procRegCloseKey, uintptr(hp))
	if err := setSZ(hp, "", "ZDownloader 种子文件"); err != nil {
		return err
	}
	hc, err := createKey(classesSub + `\` + progIDTorrent + `\shell\open\command`)
	if err != nil {
		return err
	}
	defer regCall(procRegCloseKey, uintptr(hc))
	if err := setSZ(hc, "", cmd); err != nil {
		return err
	}
	he, err := createKey(classesSub + `\.torrent`)
	if err != nil {
		return err
	}
	defer regCall(procRegCloseKey, uintptr(he))
	return setSZ(he, "", progIDTorrent)
}

// DisableTorrent 取消 .torrent 注册:仅当扩展名默认指向本程序 ProgID 时
// 才清除默认值(保留键上其他值);ProgID 键仅在本程序名下时整树删除。
func DisableTorrent() error {
	if cur, ok := querySZ(classesSub+`\.torrent`, ""); ok && cur == progIDTorrent {
		if err := deleteDefaultValue(classesSub + `\.torrent`); err != nil {
			return err
		}
	}
	if cmd, ok := querySZ(classesSub+`\`+progIDTorrent+`\shell\open\command`, ""); ok && isOurCommand(cmd) {
		return deleteTree(classesSub + `\` + progIDTorrent)
	}
	return nil
}

// Set 应用两类关联;幂等,重复开启会刷新命令中的程序路径(程序移动或
// 升级覆盖后自愈)。
func Set(magnet, torrent bool) error {
	enable := func(on bool, en func() error, dis func() error, name string) error {
		var err error
		if on {
			err = en()
		} else {
			err = dis()
		}
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	}
	if err := enable(magnet, EnableMagnet, DisableMagnet, "magnet 关联"); err != nil {
		return err
	}
	return enable(torrent, EnableTorrent, DisableTorrent, "种子文件关联")
}

// OpenSystemSettings 打开系统「默认应用」设置页。
func OpenSystemSettings() error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", "ms-settings:defaultapps").Start()
}
