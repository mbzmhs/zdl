//go:build !darwin

package main

// appBundleDataDir 非 macOS 平台无 .app 启动形态(Windows 为裸 exe、Linux
// 为命令行),数据目录沿用程序同级 data/ 规则。
func appBundleDataDir(exePath string) (string, bool) { return "", false }
