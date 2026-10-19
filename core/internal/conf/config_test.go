package conf

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// zdPath 构造跨平台测试路径:Windows 下 R:\A 形态,Unix 下 R:/A。
// RecentDirs 的去重语义(filepath.Clean + EqualFold)本就跨平台,只是原测试
// 字面量写死反斜杠,darwin 下 \ 不是分隔符、尾斜杠归一失效。
func zdPath(parts ...string) string {
	return filepath.Join(parts...)
}

// zdVariant 同一路径的小写+尾分隔符写法,用于验证去重命中。
func zdVariant(p string) string {
	return strings.ToLower(p) + string(filepath.Separator)
}

func at(h, m int) time.Time {
	return time.Date(2026, 1, 1, h, m, 0, 0, time.Local)
}

func TestRSSInWindow(t *testing.T) {
	r := DefaultRSS()
	if !r.InWindow(at(3, 0)) {
		t.Fatal("未启用时段限制时应始终允许")
	}
	r.WindowEnabled = true
	r.WindowStart, r.WindowEnd = "08:00", "18:00"
	cases := []struct {
		h, m int
		want bool
	}{{9, 0, true}, {7, 59, false}, {8, 0, true}, {18, 0, false}, {17, 59, true}}
	for _, c := range cases {
		if got := r.InWindow(at(c.h, c.m)); got != c.want {
			t.Errorf("08:00-18:00 @ %02d:%02d = %v, want %v", c.h, c.m, got, c.want)
		}
	}
	// 跨天窗口
	r.WindowStart, r.WindowEnd = "23:00", "06:00"
	for _, c := range []struct {
		h, m int
		want bool
	}{{23, 30, true}, {1, 0, true}, {5, 59, true}, {6, 0, false}, {12, 0, false}} {
		if got := r.InWindow(at(c.h, c.m)); got != c.want {
			t.Errorf("23:00-06:00 @ %02d:%02d = %v, want %v", c.h, c.m, got, c.want)
		}
	}
	// 起止相同 = 不限时段
	r.WindowStart, r.WindowEnd = "08:00", "08:00"
	if !r.InWindow(at(3, 0)) {
		t.Error("起止相同应视为不限时段")
	}
}

func TestRSSNormalize(t *testing.T) {
	c := Config{RSS: RSSConfig{IntervalMin: 1, MaxItems: 1, WindowStart: "99:99", WindowEnd: "x"}}
	c.normalize()
	if c.RSS.IntervalMin != 5 || c.RSS.MaxItems != 20 {
		t.Fatalf("下限钳制失败: %+v", c.RSS)
	}
	if c.RSS.WindowStart != "00:00" || c.RSS.WindowEnd != "23:59" {
		t.Fatalf("非法时段未回退默认: %+v", c.RSS)
	}
	c = Config{RSS: RSSConfig{IntervalMin: 99999, MaxItems: 999999, WindowStart: "07:30", WindowEnd: "22:00"}}
	c.normalize()
	if c.RSS.IntervalMin != 1440 || c.RSS.MaxItems != 5000 {
		t.Fatalf("上限钳制失败: %+v", c.RSS)
	}
	if c.RSS.WindowStart != "07:30" || c.RSS.WindowEnd != "22:00" {
		t.Fatalf("合法时段被改动: %+v", c.RSS)
	}
	if c.RSS.Interval() != 24*time.Hour {
		t.Fatalf("Interval() = %v", c.RSS.Interval())
	}
}

func TestAddRecentDir(t *testing.T) {
	var c Config
	if c.AddRecentDir("") || c.AddRecentDir("   ") {
		t.Fatal("空路径不应记录")
	}
	if len(c.RecentDirs) != 0 {
		t.Fatalf("空路径记录进了列表: %v", c.RecentDirs)
	}

	// 新路径插到最前
	for _, p := range []string{zdPath("R:", "A"), zdPath("R:", "B"), zdPath("R:", "C")} {
		c.AddRecentDir(p)
	}
	if c.RecentDirs[0] != zdPath("R:", "C") || len(c.RecentDirs) != 3 {
		t.Fatalf("最新应靠前: %v", c.RecentDirs)
	}

	// 尾分隔符与大小写:视为同一路径,去重并保留原写法提到最前
	if !c.AddRecentDir(zdVariant(zdPath("R:", "A"))) {
		t.Fatal("同路径(大小写/尾分隔符差异)应视为命中并调整排序")
	}
	if c.RecentDirs[0] != zdPath("R:", "A") || len(c.RecentDirs) != 3 {
		t.Fatalf("去重失败: %v", c.RecentDirs)
	}

	// 重复记录已是最前:内容不变
	before := append([]string(nil), c.RecentDirs...)
	if c.AddRecentDir(zdPath("R:", "A")) {
		t.Fatal("已是最前不应报变化")
	}
	if len(c.RecentDirs) != len(before) {
		t.Fatalf("重复记录改变长度: %v", c.RecentDirs)
	}

	// 超上限裁掉最旧
	var d Config
	for i := 0; i < maxRecentDirs+3; i++ {
		d.AddRecentDir(zdPath("R:", "D"+string(rune('A'+i))))
	}
	if len(d.RecentDirs) != maxRecentDirs {
		t.Fatalf("未按上限 %d 裁剪: %d", maxRecentDirs, len(d.RecentDirs))
	}
	if d.RecentDirs[0] != zdPath("R:", "DM") || d.RecentDirs[maxRecentDirs-1] != zdPath("R:", "DD") {
		t.Fatalf("裁剪应保留最新: %v", d.RecentDirs)
	}

	// copy-on-write:旧切片持有者不受影响
	hold := d.RecentDirs
	d.AddRecentDir(zdPath("R:", "New"))
	if len(hold) != maxRecentDirs || hold[0] == zdPath("R:", "New") {
		t.Fatalf("并发持有的旧切片被改动: %v", hold)
	}
}

func TestRemoveRecentDir(t *testing.T) {
	var c Config
	if c.RemoveRecentDir(zdPath("R:", "X")) {
		t.Fatal("空列表删除不应报变化")
	}
	c.AddRecentDir(zdPath("R:", "A"))
	c.AddRecentDir(zdPath("R:", "B"))
	c.AddRecentDir(zdPath("R:", "C"))
	if !c.RemoveRecentDir(zdVariant(zdPath("R:", "B"))) {
		t.Fatal("大小写/尾分隔符命中应删除成功")
	}
	if len(c.RecentDirs) != 2 || c.RecentDirs[0] != zdPath("R:", "C") || c.RecentDirs[1] != zdPath("R:", "A") {
		t.Fatalf("删除后应剩余最新在前: %v", c.RecentDirs)
	}
	if c.RemoveRecentDir(zdPath("R:", "ZZ")) || c.RemoveRecentDir("") {
		t.Fatal("删除不存在项/空参数不应报变化")
	}
	if len(c.RecentDirs) != 2 {
		t.Fatalf("无效删除不应改列表: %v", c.RecentDirs)
	}
}
