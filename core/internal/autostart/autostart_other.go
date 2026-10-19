//go:build !windows && !darwin

// Package autostart 的非 Windows/macOS 占位实现。
package autostart

import "errors"

// Enabled 恒为 false(仅 Windows 支持注册表 Run 键方式)。
func Enabled() bool { return false }

// Enable 返回不支持错误。
func Enable() error { return errors.New("当前平台不支持开机自动启动") }

// Disable 无操作。
func Disable() error { return nil }

// RefreshSelf 无操作。
func RefreshSelf() {}
