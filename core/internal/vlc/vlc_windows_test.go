//go:build windows

package vlc

import "testing"

// 依赖本机 libvlc 运行时:缺失即跳过(CI/干净环境无 VLC 是常态)。
// 有 VLC 时验证 加载→建播放器→释放 全链路不崩(不发声,不加载媒体)。
func TestLoadAndNewPlayer(t *testing.T) {
	if err := Load(); err != nil {
		t.Skipf("libvlc 不可用,跳过: %v", err)
	}
	p, err := NewPlayer()
	if err != nil {
		t.Fatalf("NewPlayer: %v", err)
	}
	if got := p.State(); got != StateNothing {
		t.Fatalf("新播放器状态 = %v, 期望 %v", got, StateNothing)
	}
	// libvlc 契约:无媒体时 get_time/get_length 返回 -1(未知);新播放器
	// 两者都必须非正值,正值说明状态污染
	if p.Time() > 0 {
		t.Fatalf("新播放器 Time 应为 0/-1,实际 %d", p.Time())
	}
	if p.Length() > 0 {
		t.Fatalf("新播放器 Length 应为 0/-1,实际 %d", p.Length())
	}
	p.Release() // 幂等:再放一次不得崩溃
	p.Release()
}

// 停止/暂停等在未播放态调用必须安全(轮询式 UI 会在切集时盲调)
func TestNoopCallsSafe(t *testing.T) {
	if err := Load(); err != nil {
		t.Skipf("libvlc 不可用,跳过: %v", err)
	}
	p, err := NewPlayer()
	if err != nil {
		t.Fatalf("NewPlayer: %v", err)
	}
	defer p.Release()
	p.Stop()
	p.Pause(true)
	p.Pause(false)
	p.TogglePause()
	p.SetTime(0)
	p.SetRate(1.0)
	p.Volume(50)
	p.GetVolume()
}
