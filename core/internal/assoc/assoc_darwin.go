//go:build darwin

// macOS 文件关联分两层:
//  1. 注册层:.app bundle 的 Info.plist 声明(CFBundleURLTypes magnet /
//     CFBundleDocumentTypes .torrent),拷入 /Applications 即随 LaunchServices
//     注册,交付的 URL/文件事件由 wails 的 OnUrlOpen/OnFileOpen 管道进入应用。
//  2. 默认层:LSSetDefaultHandlerForURLScheme / LSSetDefaultRoleHandlerForContentType
//     运行时接管默认应用——首次调用系统弹「要用 ZDownloader 打开吗?」确认框,
//     用户点确认即生效;已是默认时静默。取消关联 macOS 不允许应用代劳(系统设置改)。
//
// 状态查询用真实默认(NSWorkspace / LSCopyDefaultRoleHandlerForContentType),
// 不以「跑在 .app 里」近似。
package assoc

/*
#cgo darwin CFLAGS: -x objective-c
#cgo darwin LDFLAGS: -framework CoreServices -framework Foundation -framework AppKit
#include <stdlib.h>
#include <CoreServices/CoreServices.h>
#import <Foundation/Foundation.h>
#import <AppKit/AppKit.h>

// zdSetMagnetDefault LSSetDefaultHandlerForURLScheme("magnet"):系统首次弹确认框
static void zdSetMagnetDefault(const char *bundleID) {
	NSString *bid = [NSString stringWithUTF8String:bundleID];
	LSSetDefaultHandlerForURLScheme((__bridge CFStringRef)@"magnet",
		(__bridge CFStringRef)bid);
}

// zdSetTorrentDefault LSSetDefaultRoleHandlerForContentType(.torrent 的 UTI)
static void zdSetTorrentDefault(const char *bundleID) {
	NSString *bid = [NSString stringWithUTF8String:bundleID];
	LSSetDefaultRoleHandlerForContentType((__bridge CFStringRef)@"org.bittorrent.torrent",
		kLSRolesViewer, (__bridge CFStringRef)bid);
}

// zdMagnetDefault 查询 magnet: 的当前默认处理器是否为指定 bundle 路径
static bool zdMagnetDefault(const char *bundlePath) {
	NSURL *app = [[NSWorkspace sharedWorkspace] URLForApplicationToOpenURL:
		[NSURL URLWithString:@"magnet:?xt=urn:btih:0000000000000000000000000000000000000000"]];
	if (!app) return false;
	NSString *bp = [NSString stringWithUTF8String:bundlePath];
	return [app.path isEqualToString:bp];
}

// zdTorrentDefault 查询 org.bittorrent.torrent 的默认查看器 bundleID 是否匹配
static bool zdTorrentDefault(const char *bundleID) {
	NSString *bid = [NSString stringWithUTF8String:bundleID];
	CFStringRef h = LSCopyDefaultRoleHandlerForContentType(
		(__bridge CFStringRef)@"org.bittorrent.torrent", kLSRolesViewer);
	if (!h) return false;
	bool ok = [(__bridge NSString *)h isEqualToString:bid];
	CFRelease(h);
	return ok;
}

// zdBundleID 读 .app 的 CFBundleIdentifier
static char *zdBundleID(const char *bundlePath) {
	NSBundle *b = [NSBundle bundleWithPath:[NSString stringWithUTF8String:bundlePath]];
	if (!b || !b.bundleIdentifier) return NULL;
	return strdup(b.bundleIdentifier.UTF8String); // 调用方 free
}

// zdOpenDefaultAppsPane 打开 系统设置 → 桌面与程序坞(Ventura+ 默认应用所在面板)
static void zdOpenDefaultAppsPane(void) {
	[[NSWorkspace sharedWorkspace] openURL:
		[NSURL URLWithString:@"x-apple.systempreferences:com.apple.Desktop-Settings.extension"]];
}
*/
import "C"

import (
	"C"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unsafe"
)

// execPath 可测试覆盖。
var execPath = os.Executable

// bundlePath 返回当前进程所属 .app bundle 路径;裸二进制运行返回 ""。
func bundlePath() string {
	exe, err := execPath()
	if err != nil {
		return ""
	}
	if i := strings.Index(exe, "/Contents/MacOS/"); i > 0 {
		return exe[:i]
	}
	return ""
}

// bundleID 读 bundle 的 CFBundleIdentifier;读不到返回 ""。
func bundleID(bundle string) string {
	cid := C.zdBundleID(C.CString(bundle))
	if cid == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(cid))
	return C.GoString(cid)
}

// MagnetEnabled 报告 magnet: 的系统默认处理器是否为本 .app(真实查询)。
func MagnetEnabled() bool {
	b := bundlePath()
	if b == "" {
		return false
	}
	cs := C.CString(b)
	defer C.free(unsafe.Pointer(cs))
	return bool(C.zdMagnetDefault(cs))
}

// TorrentEnabled 报告 .torrent 的默认查看器是否为本 .app(真实查询)。
func TorrentEnabled() bool {
	b := bundlePath()
	if b == "" {
		return false
	}
	bid := bundleID(b)
	if bid == "" {
		return false
	}
	cs := C.CString(bid)
	defer C.free(unsafe.Pointer(cs))
	return bool(C.zdTorrentDefault(cs))
}

// Set 开启关联:先 lsregister 强制刷新 bundle 注册,再调 LaunchServices
// 接管默认——系统侧的确认弹窗是异步的,函数返回时默认可能尚未切换(用户
// 尚未点确认),以 MagnetEnabled/TorrentEnabled 的后续查询为准。
// 取消关联 macOS 不允许应用代劳。
func Set(magnet, torrent bool) error {
	if magnet != torrent {
		return errors.New("macOS 下两种关联随 .app 一并生效,不能单独开关")
	}
	if !magnet {
		return errors.New("macOS 下取消关联请在 系统设置 → 桌面与程序坞 中更改默认应用")
	}
	bundle := bundlePath()
	if bundle == "" {
		return errors.New("请以 ZDownloader.app 方式运行后再管理文件关联")
	}
	lsreg := "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"
	if out, err := exec.Command(lsreg, "-f", bundle).CombinedOutput(); err != nil {
		return fmt.Errorf("刷新 LaunchServices 注册失败: %w: %s", err, strings.TrimSpace(string(out)))
	}
	bid := bundleID(bundle)
	if bid == "" {
		return errors.New("无法读取 .app 的 bundle 标识(CFBundleIdentifier)")
	}
	cbid := C.CString(bid)
	defer C.free(unsafe.Pointer(cbid))
	if magnet {
		C.zdSetMagnetDefault(cbid)
	}
	if torrent {
		C.zdSetTorrentDefault(cbid)
	}
	return nil
}

// OpenSystemSettings 打开系统默认应用所在设置面板(桌面与程序坞)。
func OpenSystemSettings() error {
	C.zdOpenDefaultAppsPane()
	return nil
}
