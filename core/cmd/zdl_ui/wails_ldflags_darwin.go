//go:build wails && darwin

// Wails v2.16 darwin 前端(WailsContext.m)引用 UTType(UniformTypeIdentifiers
// 框架,macOS 11+),但其 #cgo LDFLAGS 未声明该框架,新版 SDK 下链接报
// undefined: _OBJC_CLASS_$_UTType。本文件只补框架链接声明,无任何代码;
// Wails 上游修复后可整体删除。
package main

/*
#cgo LDFLAGS: -framework UniformTypeIdentifiers
*/
import "C"
