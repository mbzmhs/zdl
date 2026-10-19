package engine

import (
	"testing"

	"zdownloader/internal/bt"
)

// TestBTClientConfigTwoLevelLimits BT 会话限速两级模型接线:
// 任务级 = 任务单独限速,缺省回退"默认每任务限速";全局是共享实例
// (下载与 HTTP 同一只,上传全部 BT 会话同一只)——旧实现把全局值折算
// 进每会话限速器,多任务时全局预算按任务数翻倍。
func TestBTClientConfigTwoLevelLimits(t *testing.T) {
	cfg := testConf(t.TempDir())
	cfg.GlobalLimit = 100
	cfg.TaskLimit = 50
	cfg.BT.UPSpeed = 70
	m := NewManager(cfg, t.TempDir(), t.TempDir())
	m.cfg = cfg

	tk := &Task{Kind: KindBT, SpeedLimit: 0, UPLimit: 0}
	c := m.btClientConfig(tk)
	if c.DownLimit != 50 {
		t.Fatalf("任务级下载应回退默认每任务限速 50, got %d", c.DownLimit)
	}
	if c.UpLimit != 0 {
		t.Fatalf("任务级上传未单独限速时应为 0(不设任务级上限), got %d", c.UpLimit)
	}
	if c.GlobalDown != m.gLim || c.GlobalUp != m.btULim {
		t.Fatal("会话未接入共享全局限速器")
	}
	if m.gLim.Rate() != 100 || m.btULim.Rate() != 70 {
		t.Fatalf("全局限速器初值不符: %d/%d", m.gLim.Rate(), m.btULim.Rate())
	}

	// 任务单独限速时不回退
	tk.SpeedLimit, tk.UPLimit = 20, 30
	c = m.btClientConfig(tk)
	if c.DownLimit != 20 || c.UpLimit != 30 {
		t.Fatalf("任务级限速应保持任务值: %d/%d", c.DownLimit, c.UpLimit)
	}

	// 热更新任务限速:SetLimits 只动任务级,全局实例不受牵连
	cfg2 := m.cfg
	cfg2.BT.UPSpeed = 90
	if err := m.UpdateConfig(cfg2); err != nil {
		t.Fatal(err)
	}
	if m.btULim.Rate() != 90 {
		t.Fatalf("UpdateConfig 未热更新 BT 全局上传限速: %d", m.btULim.Rate())
	}
	if c := m.btClientConfig(tk); c.UpLimit != 30 {
		t.Fatalf("任务级上传限速不应被全局折算: %d", c.UpLimit)
	}
}

// 编译期守卫:ClientConfig 共享限速字段存在且为指针类型。
var _ = bt.ClientConfig{GlobalDown: nil, GlobalUp: nil}
