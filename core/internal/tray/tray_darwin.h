// tray_darwin.h — macOS 托盘 cgo 桥接声明。
// 实现在 tray_darwin.m;前导码只做声明(带 //export 时前导码被编译两遍,
// 实现放前导码会重复符号)。
#import <Cocoa/Cocoa.h>

// Go 侧回调(cgo //export 生成)
extern void zdTrayOpen(void);
extern void zdTraySettings(void);
extern void zdTrayExit(void);

// 在主线程创建状态栏项。icon/tooltip 为 C 侧持有的堆内存(块内释放)。
void zdTraySetupC(const char *icon, int iconLen, const char *tooltip);

// 主线程移除状态栏项。
void zdTrayShutdownC(void);
