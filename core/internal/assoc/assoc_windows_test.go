//go:build windows

package assoc

import (
	"os"
	"testing"
)

// 沙箱键路径:测试期间所有注册表读写都落在 ZDLAssocTest 之下,
// 结束后整树删除,不触碰真实 Classes 键。
const sandbox = `Software\Classes\ZDLAssocTest`

func TestMain(m *testing.M) {
	classesSub = sandbox + `\Classes`
	torrentUCSub = sandbox + `\uc\.torrent\UserChoice`
	magnetUCSubFmt = sandbox + `\uc\magnet\UserChoice%s`
	code := m.Run()
	_ = deleteTree(sandbox)
	os.Exit(code)
}

func TestMagnetRoundTrip(t *testing.T) {
	if MagnetEnabled() {
		t.Fatal("沙箱初始状态应为未关联")
	}
	if err := EnableMagnet(); err != nil {
		t.Fatal(err)
	}
	if !MagnetEnabled() {
		t.Fatal("开启后状态应为已关联")
	}
	// 幂等:重复开启仍成功且状态不变
	if err := EnableMagnet(); err != nil {
		t.Fatal(err)
	}
	if !MagnetEnabled() {
		t.Fatal("重复开启后状态应保持已关联")
	}
	if err := DisableMagnet(); err != nil {
		t.Fatal(err)
	}
	if MagnetEnabled() {
		t.Fatal("关闭后状态应为未关联")
	}
	// 重复关闭不报错
	if err := DisableMagnet(); err != nil {
		t.Fatal(err)
	}
}

func TestTorrentRoundTrip(t *testing.T) {
	if err := EnableTorrent(); err != nil {
		t.Fatal(err)
	}
	if !TorrentEnabled() {
		t.Fatal("开启后状态应为已关联")
	}
	// UserChoice 冲突:系统已指定其他默认程序时,关联实际未生效
	if err := func() error {
		h, err := createKey(torrentUCSub)
		if err != nil {
			return err
		}
		defer regCall(procRegCloseKey, uintptr(h))
		return setSZ(h, "ProgId", "OtherClient.torrent")
	}(); err != nil {
		t.Fatal(err)
	}
	if TorrentEnabled() {
		t.Fatal("UserChoice 指向其他程序时状态应为未生效")
	}
	// 冲突解除(模拟用户在系统里改选本程序)后恢复已关联
	if err := func() error {
		h, err := createKey(torrentUCSub)
		if err != nil {
			return err
		}
		defer regCall(procRegCloseKey, uintptr(h))
		return setSZ(h, "ProgId", progIDTorrent)
	}(); err != nil {
		t.Fatal(err)
	}
	if !TorrentEnabled() {
		t.Fatal("UserChoice 指向本程序后状态应为已关联")
	}
	if err := DisableTorrent(); err != nil {
		t.Fatal(err)
	}
	if TorrentEnabled() {
		t.Fatal("关闭后状态应为未关联")
	}
}

func TestDisableLeavesForeignRegistration(t *testing.T) {
	// 其他 BT 客户端的 magnet 注册:关闭操作不得改动
	magnetSub := classesSub + `\magnet`
	h, err := createKey(magnetSub + `\shell\open\command`)
	if err != nil {
		t.Fatal(err)
	}
	if err := setSZ(h, "", `"C:\Program Files\OtherClient\other.exe" "%1"`); err != nil {
		t.Fatal(err)
	}
	regCall(procRegCloseKey, uintptr(h))
	if err := DisableMagnet(); err != nil {
		t.Fatal(err)
	}
	if cur, ok := querySZ(magnetSub+`\shell\open\command`, ""); !ok || cur == "" {
		t.Fatal("他方注册的命令不应被删除")
	}
	_ = deleteTree(magnetSub)
}

func TestSetAppliesBoth(t *testing.T) {
	if err := Set(true, true); err != nil {
		t.Fatal(err)
	}
	if !MagnetEnabled() || !TorrentEnabled() {
		t.Fatal("Set(true,true) 后两者都应为已关联")
	}
	if err := Set(false, false); err != nil {
		t.Fatal(err)
	}
	if MagnetEnabled() || TorrentEnabled() {
		t.Fatal("Set(false,false) 后两者都应为未关联")
	}
}
