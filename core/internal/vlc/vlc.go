// Package vlc 提供 libvlc 的纯 Go 绑定(v3 原生 UI 播放内核)。
//
// 平台拆分:Windows 用 syscall.LoadLibraryEx 纯标准库实现(本文件,零 cgo/
// 零第三方);Unix 用 purego dlopen(vlc_unix.go)。API 面刻意收窄为轮询式
// 状态机:get_state/get_time/get_length 由 UI 定时器拉取,不用事件回调——
// 回调会从 VLC 线程打回 Go,纯 Go 绑定下跨线程回调的安全成本远高于轮询。
//
// 线程契约(重要,均由 spike 实测得出):
//   - SetRenderWindow 传入的 HWND 必须由"持续泵消息的线程"拥有(如 Fyne 主
//     线程)。VLC vout 线程会向该窗口发同步消息,创建线程不泵消息=窗口未
//     响应、Stop 死锁。
//   - Stop 与 Release 会同步 join VLC 内部线程;调用方须保证在子窗口销毁前
//     先 Stop,先 Stop 再 Release(倒序=use-after-free,已实测崩溃)。
//   - Play/Pause/Time 等查询可从任意 goroutine 调用。
package vlc

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"unsafe"
)

// State 对齐 libvlc_state_t
type State int32

const (
	StateNothing State = iota // 空闲
	StateOpening              // 打开媒体
	StateBuffering            // 缓冲中
	StatePlaying              // 播放中
	StatePaused               // 已暂停
	StateStopped              // 已停止
	StateEnded                // 自然播完
	StateError                // 出错(编解码/源不可达)
)

func (s State) String() string {
	switch s {
	case StateNothing:
		return "空闲"
	case StateOpening:
		return "打开中"
	case StateBuffering:
		return "缓冲中"
	case StatePlaying:
		return "播放中"
	case StatePaused:
		return "已暂停"
	case StateStopped:
		return "已停止"
	case StateEnded:
		return "已播完"
	case StateError:
		return "出错"
	}
	return "未知"
}

// ErrNoLibVLC 运行时未找到 libvlc。加载失败不是致命错误:播放页据此降级为
// 引导提示(引导安装 VLC 或把 libvlc.dll 放程序目录),其余功能不受影响。
var ErrNoLibVLC = errors.New("未找到 libvlc 运行时(安装 VLC 或将 libvlc.dll 置于程序目录)")

var (
	dllHandle  uintptr // LoadLibraryEx 返回的模块句柄;0=未加载
	dllOnceErr error   // 加载失败原因(复用给后续 Load 调用)
)

// Load 按"程序目录 → VLC 安装目录 → PATH"顺序定位并加载 libvlc.dll。
// 必须用 LoadLibraryEx + LOAD_WITH_ALTERED_SEARCH_PATH:libvlc.dll 依赖同
// 目录的 libvlccore.dll 与 plugins/,普通 LoadLibrary 不搜 DLL 自身目录,
// 程序从任意工作目录启动都会报 126。幂等:重复调用直接返回首次结果。
func Load() error {
	if dllHandle != 0 {
		return nil
	}
	if dllOnceErr != nil {
		return dllOnceErr
	}
	dllOnceErr = loadLibvlc()
	return dllOnceErr
}

// loadLibvlc 候选路径遍历(平台中立);openLibvlc 由各平台文件提供
func loadLibvlc() error {
	candidates := []string{
		`C:\Program Files\VideoLAN\VLC\libvlc.dll`,
		`C:\Program Files (x86)\VideoLAN\VLC\libvlc.dll`,
		"libvlc.dll", // PATH 搜索兜底(VLC 安装器会把目录加入 App Paths)
	}
	if exe, err := os.Executable(); err == nil {
		// 程序目录优先:便携部署(发行包带 libvlc 运行时)不依赖本机装 VLC
		candidates = append([]string{filepath.Join(filepath.Dir(exe), "libvlc.dll")}, candidates...)
	}
	for _, p := range candidates {
		if p != "libvlc.dll" {
			if _, err := os.Stat(p); err != nil {
				continue
			}
		}
		h, err := openLibvlc(p)
		if err == nil && h != 0 {
			dllHandle = h
			return nil
		}
	}
	return ErrNoLibVLC
}

// cstr 转 NUL 结尾 UTF-8。返回的 uintptr 只允许"在同一个调用表达式内"传给
// Proc.Call(syscall 的 uintptrescapes 约定会保活);不得存起来跨语句使用。
func cstr(s string) uintptr {
	b := append([]byte(s), 0)
	return uintptr(unsafe.Pointer(&b[0]))
}

// Player 一个媒体播放器(封装 media_player)。并发安全:内部调用均为
// libvlc 线程安全接口,Time/State 轮询与 UI 定时器同拍。
type Player struct {
	mp   uintptr // libvlc_media_player_t*
	inst uintptr // libvlc_instance_t*(随 player 一起释放)
}

// NewPlayer 创建播放器。要求 Load() 已成功。
func NewPlayer() (*Player, error) {
	if dllHandle == 0 {
		return nil, ErrNoLibVLC
	}
	inst, _ := callVLC("libvlc_new", 0, 0)
	if inst == 0 {
		return nil, fmt.Errorf("libvlc_new 失败")
	}
	mp, _ := callVLC("libvlc_media_player_new", inst)
	if mp == 0 {
		callVLC("libvlc_release", inst)
		return nil, fmt.Errorf("libvlc_media_player_new 失败")
	}
	// instance 引用挂在 player 上:Release 时先 mp 后 instance,VLC 内部
	// 持有 instance 引用计数,顺序颠倒同样安全,但按官方示例顺序收尾
	return &Player{mp: mp, inst: inst}, nil
}

// SetRenderWindow 绑定视频渲染窗口(Windows HWND)。
// 必须在 PlayFile 之前调用;HWND 归 UI 线程所有并持续泵消息(见包注释)。
// hwnd 为 0 表示纯音频(或解除绑定)。
func (p *Player) SetRenderWindow(hwnd uintptr) {
	if p.mp == 0 || hwnd == 0 {
		return
	}
	callVLC("libvlc_media_player_set_hwnd", p.mp, hwnd)
}

// PlayFile 播放本地文件路径(HTTP/S 源用 PlayURL)。
func (p *Player) PlayFile(path string) error { return p.playPath(path, false) }

// PlayURL 播放网络源(边下边播的流端点 URL 即走这里,libvlc 自带
// HTTP Range/重定向支持,边下边播管线无需任何改动)。
func (p *Player) PlayURL(url string) error { return p.playPath(url, true) }

func (p *Player) playPath(s string, isURL bool) error {
	if p.mp == 0 {
		return ErrNoLibVLC
	}
	var media uintptr
	if isURL {
		media, _ = callVLC("libvlc_media_new_location", p.inst, cstr(s))
	} else {
		media, _ = callVLC("libvlc_media_new_path", p.inst, cstr(s))
	}
	if media == 0 {
		return fmt.Errorf("libvlc_media_new_%s 失败: %s", map[bool]string{true: "location", false: "path"}[isURL], s)
	}
	// 顺序不可倒置:set_media 会 retain,之后才允许 release 我们的引用
	// (倒置=media 引用归零悬垂,play 必崩——spike 实测)
	callVLC("libvlc_media_player_set_media", p.mp, media)
	callVLC("libvlc_media_release", media)
	rc, _ := callVLC("libvlc_media_player_play", p.mp)
	if int32(rc) < 0 {
		return fmt.Errorf("libvlc_media_player_play 失败: %d", int32(rc))
	}
	return nil
}

// Stop 同步停止播放(join 内部线程,耗时可达数百毫秒)。调用方线程契约:
// 若绑定了渲染窗口,Stop 必须发生在窗口销毁之前;是否要求在 UI 线程执行
// 由调用方决定(建议与 DestroyWindow 同一 UI 派发以避免消息竞态)。
func (p *Player) Stop() {
	if p.mp == 0 {
		return
	}
	callVLC("libvlc_media_player_stop", p.mp) // VLC3 无 stop_async
}

// Pause 暂停/恢复。on=true 暂停。
func (p *Player) Pause(on bool) {
	if p.mp == 0 {
		return
	}
	v := uintptr(0)
	if on {
		v = 1
	}
	callVLC("libvlc_media_player_set_pause", p.mp, v)
}

// TogglePause 翻转暂停态(按当前状态判断,避免 UI 与实际状态漂移)。
func (p *Player) TogglePause() {
	if p.mp == 0 {
		return
	}
	switch p.State() {
	case StatePlaying:
		p.Pause(true)
	case StatePaused:
		p.Pause(false)
	}
}

// Time 当前播放位置(毫秒;流媒体源可能返回 0 或 -1)
func (p *Player) Time() int64 {
	if p.mp == 0 {
		return 0
	}
	t, _ := callVLC("libvlc_media_player_get_time", p.mp)
	return int64(t)
}

// SetTime 跳转(毫秒)。libvlc 对网络源做 Range 重定向,与边下边播的
// seek 语义天然契合:引擎收到的 Range 起点即跳转字节。
func (p *Player) SetTime(ms int64) {
	if p.mp == 0 {
		return
	}
	callVLC("libvlc_media_player_set_time", p.mp, uintptr(ms))
}

// Length 媒体总时长(毫秒;元数据就绪前为 0)
func (p *Player) Length() int64 {
	if p.mp == 0 {
		return 0
	}
	l, _ := callVLC("libvlc_media_player_get_length", p.mp)
	return int64(l)
}

// State 当前状态
func (p *Player) State() State {
	if p.mp == 0 {
		return StateNothing
	}
	s, _ := callVLC("libvlc_media_player_get_state", p.mp)
	return State(int32(s))
}

// SetRate 倍速(1.0=原速;范围与合法性由 libvlc 校验,越界自动钳制)
func (p *Player) SetRate(r float64) {
	if p.mp == 0 {
		return
	}
	// C float 按位传入:uintptr 承载 float32 的 IEEE 位型
	callVLC("libvlc_media_player_set_rate", p.mp, uintptr(math.Float32bits(float32(r))))
}

// Rate 当前倍速
func (p *Player) Rate() float64 {
	if p.mp == 0 {
		return 1
	}
	v, _ := callVLC("libvlc_media_player_get_rate", p.mp)
	return float64(math.Float32frombits(uint32(v)))
}

// Volume 设置音量(0-100;>100 由 libvlc 按软件增益放大,UI 侧钳制到 100)
func (p *Player) Volume(v int) {
	if p.mp == 0 {
		return
	}
	if v < 0 {
		v = 0
	}
	callVLC("libvlc_audio_set_volume", p.mp, uintptr(v))
}

// Volume 获取当前音量(-1 表示失败)
func (p *Player) GetVolume() int {
	if p.mp == 0 {
		return -1
	}
	v, _ := callVLC("libvlc_audio_get_volume", p.mp)
	return int(int32(v))
}

// Release 释放播放器与 libvlc 实例。契约:先 Stop 再 Release;Release 后
// 本对象不可再用(幂等保护防双调)。
func (p *Player) Release() {
	if p.mp == 0 {
		return
	}
	callVLC("libvlc_media_player_release", p.mp)
	if p.inst != 0 {
		callVLC("libvlc_release", p.inst)
	}
	p.mp, p.inst = 0, 0
}
