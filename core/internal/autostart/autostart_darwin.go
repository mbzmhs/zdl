//go:build darwin

// Package autostart 的 macOS 实现:写 ~/Library/LaunchAgents/com.zdownloader.plist
// (LaunchAgent,RunAtLoad 登录即拉起,裸启动静默常驻——zdl_shell 本就是托盘
// 组件,不带参数,与 Windows 注册表 Run 键同语义)。plist 即唯一事实来源,
// 不写 config.json;用户无需管理员权限。
// 裸二进制(非 .app)同样可用;程序移动/升级后路径漂移由 RefreshSelf 自愈。
package autostart

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// agentLabel LaunchAgent 的 Label,亦即 plist 文件名(去掉扩展名)。
const agentLabel = "com.zdownloader"

// launchAgentsDir 可在测试中覆盖为临时目录,避免触碰真实 LaunchAgents。
var launchAgentsDir = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home + "/Library/LaunchAgents"
}

func plistPath() (string, error) {
	dir := launchAgentsDir()
	if dir == "" {
		return "", errors.New("无法确定用户主目录")
	}
	return dir + "/" + agentLabel + ".plist", nil
}

// commandArgs 启动命令:登记目标恒为同目录的 zdl_shell(常驻托盘组件)——
// 自启开关在窗口设置页触发,调用方是可退出的 zdl_ui,直登 UI 会让登录后
// 先弹窗口;托盘才是静默常驻的正确入口,窗口由召唤链按需拉起。找不到兄弟
// 文件时(历史单二进制布局/测试环境)回落到调用方自身,保持旧行为(与
// autostart_windows.go 的 commandString 同语义)。不带参数:zdl_shell 恒为
// 常驻托盘,裸启动即其本征形态。
func commandArgs() ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if shell := filepath.Join(filepath.Dir(exe), "zdl_shell"); fileExists(shell) {
		exe = shell
	}
	return []string{exe}, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// plistContent 渲染 LaunchAgent plist XML。路径等参数值经 XML 转义。
func plistContent(args []string) string {
	esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	b.WriteString("  <key>Label</key>\n  <string>" + esc(agentLabel) + "</string>\n")
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, a := range args {
		b.WriteString("    <string>" + esc(a) + "</string>\n")
	}
	b.WriteString("  </array>\n")
	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// Enabled 报告当前是否已开启开机自启(plist 存在)。
func Enabled() bool {
	p, err := plistPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Enable 开机自启;重复调用幂等(覆盖为最新内容)。
func Enable() error {
	args, err := commandArgs()
	if err != nil {
		return err
	}
	dir := launchAgentsDir()
	if dir == "" {
		return errors.New("无法确定用户主目录")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建 LaunchAgents 目录失败: %w", err)
	}
	p, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(plistContent(args)), 0o644); err != nil {
		return fmt.Errorf("写入 LaunchAgent 失败: %w", err)
	}
	return nil
}

// Disable 关闭开机自启;plist 不存在视为已关闭。
func Disable() error {
	p, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除 LaunchAgent 失败: %w", err)
	}
	return nil
}

// RefreshSelf 在程序启动时调用:若已开启自启但 plist 内容与当前程序
// 不一致(移动目录/升级覆盖),静默改写为新路径。
func RefreshSelf() {
	if !Enabled() {
		return
	}
	p, err := plistPath()
	if err != nil {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	args, err := commandArgs()
	if err != nil {
		return
	}
	if string(data) != plistContent(args) {
		_ = Enable()
	}
}
