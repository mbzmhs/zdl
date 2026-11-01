// floatwin_darwin.h — macOS 悬浮窗 cgo 桥接声明(实现见 floatwin_darwin.m)。
// 带导出函数的包其 cgo 前导码被编译两遍,实现必须拆独立 .h/.m(托盘同款教训)。
#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#import <stdlib.h>

// Go 侧回调(cgo //export 生成)
extern void zdFloatOnMove(int x, int y, int w, int h);
extern void zdFloatOnOpen(void);
extern void zdFloatMenuOpen(void);
extern void zdFloatMenuSettings(void);
extern void zdFloatMenuExit(void);

// 主线程创建面板并加载 url(x/y/w/h = 记忆位置,w/h 为 0 用默认;posSet=是否记忆有效)
void zdFloatCreateC(const char *url, int x, int y, int w, int h, int posSet);

// 主线程显隐(1=显示 0=隐藏);面板未创建时为空操作
void zdFloatSetVisibleC(int on);

// 主线程同步收起悬浮窗(退出流程用)
void zdFloatCloseC(void);
