//go:build windows

package autostart

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// 在临时子键上做 Enable/查询/Disable 往返,不触碰真实 Run 键。
func TestEnableDisableRoundtrip(t *testing.T) {
	oldSubKey, oldValueName := runSubKey, valueName
	runSubKey = `Software\ZDownloaderAutostartTest`
	valueName = "roundtrip"
	t.Cleanup(func() {
		runSubKey, valueName = oldSubKey, oldValueName
		// 删除整个测试键,不在注册表留空壳
		var h syscall.Handle
		if regCall(procRegOpenKeyExW, hkeyCurrentUser,
			uintptr(unsafe.Pointer(utf16Ptr(`Software`))),
			0, uintptr(keySetValue), uintptr(unsafe.Pointer(&h))) == errSuccess {
			defer regCall(procRegCloseKey, uintptr(h))
			regCall(procRegDeleteTreeW, uintptr(h),
				uintptr(unsafe.Pointer(utf16Ptr(`ZDownloaderAutostartTest`))))
		}
	})

	if Enabled() {
		t.Fatal("测试键尚未写入,不应报告已开启")
	}
	if err := Enable(); err != nil {
		t.Fatalf("Enable 失败: %v", err)
	}
	if !Enabled() {
		t.Fatal("Enable 后 Enabled 应为 true")
	}
	if err := Enable(); err != nil { // 幂等:覆盖写
		t.Fatalf("重复 Enable 失败: %v", err)
	}
	if err := Disable(); err != nil {
		t.Fatalf("Disable 失败: %v", err)
	}
	if Enabled() {
		t.Fatal("Disable 后 Enabled 应为 false")
	}
	// 再删一次应视为已关闭而非报错
	if err := Disable(); err != nil {
		t.Fatalf("重复 Disable 应幂等: %v", err)
	}
}

// 启动命令必须带引号(路径含空格)且附 -no-browser。
func TestCommandStringQuoted(t *testing.T) {
	cmd, err := commandString()
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	if !strings.HasPrefix(cmd, `"`+exe+`"`) {
		t.Fatalf("命令应以带引号的 exe 路径开头: %q", cmd)
	}
	if !strings.HasSuffix(cmd, "-no-browser") {
		t.Fatalf("命令应附 -no-browser: %q", cmd)
	}
}
