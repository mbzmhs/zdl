//go:build darwin

package assoc

import (
	"strings"
	"testing"
)

// bundlePath 识别:.app 内运行返回 bundle,裸二进制返回空。
func TestBundlePath(t *testing.T) {
	old := execPath
	t.Cleanup(func() { execPath = old })

	cases := map[string]string{
		"/Applications/ZDownloader.app/Contents/MacOS/zdownloader": "/Applications/ZDownloader.app",
		"/tmp/dist/zdownloader": "",
	}
	for exe, want := range cases {
		exe, want := exe, want
		execPath = func() (string, error) { return exe, nil }
		if got := bundlePath(); got != want {
			t.Fatalf("bundlePath(%q) = %q, 期望 %q", exe, got, want)
		}
	}
}

// Set 语义:裸二进制报打包提示;单独开关报不支持;.app 内开关联需走 lsregister
// (真 lsregister 调用不在单测覆盖,由实机验证)。
func TestSetSemantics(t *testing.T) {
	old := execPath
	t.Cleanup(func() { execPath = old })
	execPath = func() (string, error) { return "/tmp/dist/zdownloader", nil }

	if err := Set(true, true); err == nil || !strings.Contains(err.Error(), "app") {
		t.Fatalf("裸二进制应提示需 .app 运行: %v", err)
	}
	if err := Set(false, false); err == nil {
		t.Fatal("取消关联应报系统设置提示")
	}
	if err := Set(true, false); err == nil || !strings.Contains(err.Error(), "不能单独开关") {
		t.Fatalf("单独开关应报不支持: %v", err)
	}
}
