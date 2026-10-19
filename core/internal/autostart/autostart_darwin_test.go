//go:build darwin

package autostart

import (
	"os"
	"strings"
	"testing"
)

// 在临时目录做 Enable/查询/Disable 往返,不触碰真实 LaunchAgents。
func TestEnableDisableRoundtrip(t *testing.T) {
	oldDir := launchAgentsDir
	tmp := t.TempDir()
	launchAgentsDir = func() string { return tmp }
	t.Cleanup(func() { launchAgentsDir = oldDir })

	if Enabled() {
		t.Fatal("临时目录尚未写入,不应报告已开启")
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
	data, err := os.ReadFile(tmp + "/" + agentLabel + ".plist")
	if err != nil {
		t.Fatalf("读取 plist 失败: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "<key>RunAtLoad</key>") || !strings.Contains(content, "<true/>") {
		t.Fatalf("plist 应含 RunAtLoad=true: %s", content)
	}
	exe, _ := os.Executable()
	if !strings.Contains(content, "<string>"+exe+"</string>") {
		t.Fatalf("plist 应含当前程序路径: %s", content)
	}
	if !strings.Contains(content, "<string>--no-browser</string>") {
		t.Fatalf("plist 应附 --no-browser: %s", content)
	}

	// RefreshSelf 自愈:内容被破坏后应被改写回最新路径
	if err := os.WriteFile(tmp+"/"+agentLabel+".plist", []byte("<outdated/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	RefreshSelf()
	data, err = os.ReadFile(tmp + "/" + agentLabel + ".plist")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content {
		t.Fatal("RefreshSelf 应把 plist 改写回最新内容")
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

// plistContent 应转义 XML 特殊字符(路径可能含 &)。
func TestPlistContentEscape(t *testing.T) {
	out := plistContent([]string{"/a&b/c", "--no-browser"})
	if strings.Contains(out, "/a&b/c") {
		t.Fatalf("& 应被转义: %s", out)
	}
	if !strings.Contains(out, "/a&amp;b/c") {
		t.Fatalf("应写入 &amp; 实体: %s", out)
	}
}
