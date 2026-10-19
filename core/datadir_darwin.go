//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
)

// appBundleDataDir 判定 macOS .app 启动形态的数据目录。
// .app 的可执行文件位于 Contents/MacOS/ 下,若沿用「程序同级 data/」规则,
// 日志/任务状态/配置会写进 bundle 内部(Contents/MacOS/data):
//  1. 每次运行都改动 app 包内容,发布物(zip/签名)被污染——codesign 递归
//     签名 bundle 时直接失败;
// 2. app 包应视为只读分发物,运行时状态放用户配置目录才符合 macOS 惯例。
// 故 .app 启动统一改用 ~/Library/Application Support/ZDownloader。
// 兼容迁移:新目录尚不存在、旧版本曾在包内留下 data/(Contents/MacOS/data)
// 时,整体 rename 到新位置,历史任务/配置不丢失。
// 非 .app 形态(裸二进制)返回 ok=false,走既有的程序同级 data/ 规则。
func appBundleDataDir(exePath string) (string, bool) {
	parts := strings.Split(exePath, string(filepath.Separator))
	if len(parts) < 4 {
		return "", false
	}
	if parts[len(parts)-2] != "MacOS" || parts[len(parts)-3] != "Contents" ||
		!strings.HasSuffix(parts[len(parts)-4], ".app") {
		return "", false
	}
	base, err := os.UserConfigDir() // ~/Library/Application Support
	if err != nil {
		return "", false
	}
	dataDir := filepath.Join(base, "ZDownloader")
	oldDir := filepath.Join(filepath.Dir(exePath), "data") // 旧版本包内数据
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		migrated := false
		if _, err := os.Stat(oldDir); err == nil {
			migrated = os.MkdirAll(base, 0o755) == nil && os.Rename(oldDir, dataDir) == nil
		}
		if !migrated {
			// 目录必须在此建好:setupLogging 紧随其后按该路径开日志文件,
			// 目录不存在则 O_CREATE 失败被静默吞掉,日志文件永远不落盘
			_ = os.MkdirAll(dataDir, 0o755)
		}
	}
	return dataDir, true
}
