//go:build !windows && !darwin

// Package assoc 的非 Windows/macOS 占位实现。
package assoc

import "errors"

// MagnetEnabled 恒为 false。
func MagnetEnabled() bool { return false }

// TorrentEnabled 恒为 false。
func TorrentEnabled() bool { return false }

// Set 返回不支持错误。
func Set(magnet, torrent bool) error { return errors.New("当前平台不支持文件关联") }

// OpenSystemSettings 非桌面平台无系统设置可开。
func OpenSystemSettings() error { return errors.New("当前平台不支持打开系统设置") }
