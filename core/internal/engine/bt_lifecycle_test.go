package engine

// BT 任务生命周期回归:运行中改选重启、完成态缩选账目收口、暂停态取消
// 清理、删除运行中任务清理。配套审计修复(bt.81)。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zdownloader/internal/bt"
)

// btLifecycleEnv 起一个带本地 seeder 的管理器,返回任务 id 与数据目录。
func btLifecycleEnv(t *testing.T, speedLimit int64) (m *Manager, id, folder, tasksDir string, meta *bt.MetaInfo) {
	t.Helper()
	tracker := fakeTracker(t)
	srcRoot, torrent, _ := seedSource(t)
	var err error
	meta, err = bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatal(err)
	}
	torrent, err = bt.BuildTorrentBytes(meta, tracker.URL+"/announce", "")
	if err != nil {
		t.Fatal(err)
	}
	meta, err = bt.ParseTorrent(torrent)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := bt.NewSeedClient(meta, srcRoot, bt.ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.Start()
	t.Cleanup(seed.Close)

	dl := t.TempDir()
	cfg := testConf(dl)
	cfg.BT.AutoSeed = false
	cfg.BT.PublicTrackers = nil
	tasksDir = filepath.Join(t.TempDir(), "tasks")
	m = NewManager(cfg, filepath.Join(t.TempDir(), "cfg.json"), tasksDir)
	ctx, cancel := context.WithCancel(context.Background())
	// 收尾先停管理器再等协程全退:测试的 TempDir 清理与后台持久化
	// (ticker 每 2s 落盘)竞态会让 unlinkat 报 not-empty(实测)
	t.Cleanup(func() {
		cancel()
		sctx, scancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer scancel()
		m.Shutdown(sctx)
	})
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	views, errs := m.AddBT(CreateReq{TorrentB64: base64.StdEncoding.EncodeToString(torrent)})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id = views[0].ID
	folder = filepath.Join(dl, views[0].Filename)
	if speedLimit > 0 {
		if err := m.Update(id, speedLimit, 0); err != nil {
			t.Fatal(err)
		}
	}
	return m, id, folder, tasksDir, meta
}

func btFileLength(meta *bt.MetaInfo, path string) int64 {
	for _, f := range meta.Files {
		if strings.Join(f.Path, "/") == path {
			return f.Length
		}
	}
	return -1
}

// btWaitDir 等会话把数据目录落成。目录由会话协程异步创建,Running 状态
// 先于 mkdir;Pause 等后续操作若不等目录,快调度下会踩空(实测)。
func btWaitDir(t *testing.T, folder string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(folder); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("会话未创建数据目录: %s", folder)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestBTChooseWhileRunningRestartsSession 运行中改选:旧会话必须先停
// (cancel ctx)再按新选择重启。旧实现只改状态不停会话,新旧两个会话
// 并发写同一批文件,旧会话按旧选择把整个种子下完并覆盖完成账目
// (实测终态 pieces=4/1、"BT 任务完成"日志来自旧会话)。
func TestBTChooseWhileRunningRestartsSession(t *testing.T) {
	m, id, _, _, meta := btLifecycleEnv(t, 4096) // 限速:保证改选落在 running 态
	if v := waitForStatus(t, m, id, StatusRunning, 15*time.Second); v.Size == 0 {
		t.Fatalf("任务未运行: %+v", v)
	}
	time.Sleep(time.Second)

	wantSize := btFileLength(meta, "doc.txt")
	if _, err := m.ChooseBTFiles(id, []string{"doc.txt"}, false); err != nil {
		t.Fatalf("改选失败: %v", err)
	}

	// 等新会话按新选择完成;账目必须自洽(旧实现终态 pieces=4/1)
	deadline := time.Now().Add(30 * time.Second)
	for {
		v := m.get(id).View()
		if v.Status == StatusCompleted || v.Status == StatusSeeding {
			if v.PiecesDone != v.PiecesTotal || v.Size != wantSize || v.Arrived > v.Size {
				t.Fatalf("改选重启后账目不自洽: pieces=%d/%d size=%d arrived=%d (want size=%d)",
					v.PiecesDone, v.PiecesTotal, v.Size, v.Arrived, wantSize)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("改选后未按新选择完成: %+v", m.get(id).View())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestBTChooseShrinkCompletedClampsArrived 完成态缩选:就地改账时 arrived
// 必须夹进新口径,否则详情页出现"已下 50KB / 大小 15B"式倒挂(实测)。
func TestBTChooseShrinkCompletedClampsArrived(t *testing.T) {
	m, id, _, _, meta := btLifecycleEnv(t, 0)
	v := waitForStatus(t, m, id, StatusCompleted, 30*time.Second)
	if v.Arrived == 0 {
		t.Fatalf("完成态 arrived 异常: %+v", v)
	}
	if _, err := m.ChooseBTFiles(id, []string{"doc.txt"}, false); err != nil {
		t.Fatalf("缩选失败: %v", err)
	}
	wantSize := btFileLength(meta, "doc.txt")
	v = m.get(id).View()
	if v.Arrived > v.Size || v.Size != wantSize || v.PiecesDone != v.PiecesTotal {
		t.Fatalf("缩选后账目未收口: size=%d arrived=%d pieces=%d/%d (want size=%d)",
			v.Size, v.Arrived, v.PiecesDone, v.PiecesTotal, wantSize)
	}
}

// TestBTCancelFromPausedCleansData 暂停态取消:与运行中取消同语义——
// 就地半成品与续传状态一起作废。旧实现从暂停取消只改状态,数据目录
// 遗留,重新开始时又被扫描恢复,同一个取消按钮两种结局。
func TestBTCancelFromPausedCleansData(t *testing.T) {
	m, id, folder, tasksDir, _ := btLifecycleEnv(t, 1) // 1 B/s:必然停在半成品
	if v := waitForStatus(t, m, id, StatusRunning, 15*time.Second); v.Size == 0 {
		t.Fatalf("任务未运行: %+v", v)
	}
	btWaitDir(t, folder)
	if err := m.Pause(id); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, id, StatusPaused, 10*time.Second)

	// 暂停态先落一版非空位图,验证取消会把状态与数据一起作废
	task := m.get(id)
	task.mu.Lock()
	task.Bitfield = "AAAA"
	task.BlockBits = "BBBB"
	task.mu.Unlock()
	m.persist(task)

	if err := m.Cancel(id); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, id, StatusCanceled, 10*time.Second)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(folder); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("暂停态取消后数据目录未删除")
		}
		time.Sleep(50 * time.Millisecond)
	}
	task = m.get(id)
	task.mu.Lock()
	bf, bb := task.Bitfield, task.BlockBits
	// 进度账目同作废:数据已丢弃,旧账留着会让"重新下载"的进度条先跳到
	// 旧位置、等 pump 首个 tick 才被会话实测值校正归零(用户实测)
	arr, dl, pd := task.Arrived, task.Downloaded, task.PiecesDone
	task.mu.Unlock()
	if bf != "" || bb != "" {
		t.Fatalf("取消后任务内续传状态未作废: %q %q", bf, bb)
	}
	if arr != 0 || dl != 0 || pd != 0 {
		t.Fatalf("取消后进度账目未清零: arrived=%d downloaded=%d piecesDone=%d", arr, dl, pd)
	}
	var saved struct {
		Bitfield   string `json:"bitfield"`
		BlockBits  string `json:"blockBits"`
		Arrived    int64  `json:"arrived"`
		Downloaded int64  `json:"downloaded"`
		PiecesDone int    `json:"piecesDone"`
	}
	raw, err := os.ReadFile(filepath.Join(tasksDir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Bitfield != "" || saved.BlockBits != "" {
		t.Fatalf("落盘 JSON 仍残留续传状态: %q %q", saved.Bitfield, saved.BlockBits)
	}
	if saved.Arrived != 0 || saved.Downloaded != 0 || saved.PiecesDone != 0 {
		t.Fatalf("落盘 JSON 仍残留进度账目: arrived=%d downloaded=%d piecesDone=%d",
			saved.Arrived, saved.Downloaded, saved.PiecesDone)
	}
}

// TestBTResumeFromCanceledHealsStaleAccounting 重新开始自愈存量旧账:
// 更早版本取消的任务 JSON 里进度账目还留着旧值(数据已被取消清理),
// 不清的话"重新下载"后进度条先跳到旧位置、等 pump 首个 tick 才归零
// (用户实测)。盘上若有遗留数据被续用,pump 首个 tick 会按扫描校验
// 结果重算,不丢真进度。
func TestBTResumeFromCanceledHealsStaleAccounting(t *testing.T) {
	m, id, folder, tasksDir, _ := btLifecycleEnv(t, 1)
	if v := waitForStatus(t, m, id, StatusRunning, 15*time.Second); v.Size == 0 {
		t.Fatalf("任务未运行: %+v", v)
	}
	if err := m.Cancel(id); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, id, StatusCanceled, 10*time.Second)

	// 等取消清理把数据目录删净再继续:Windows 删除是异步的
	// (delete-pending),不等于净就返回,目录删除会与 t.TempDir 收尾
	// 竞态,间歇性判挂(实测)
	deadline := time.Now().Add(8 * time.Second)
	for {
		if _, err := os.Stat(folder); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("取消后数据目录未删除,用例不能带竞态收尾")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 模拟旧版本落盘的残留账目(取消未清):内存+JSON 同注入
	task := m.get(id)
	task.mu.Lock()
	task.Arrived, task.Downloaded, task.PiecesDone = 52_100_000, 52_100_000, 13
	task.mu.Unlock()
	m.persist(task)

	if err := m.Resume(id); err != nil {
		t.Fatal(err)
	}
	task = m.get(id)
	task.mu.Lock()
	arr, dl, pd := task.Arrived, task.Downloaded, task.PiecesDone
	task.mu.Unlock()
	if arr != 0 || dl != 0 || pd != 0 {
		t.Fatalf("重新开始未清旧账: arrived=%d downloaded=%d piecesDone=%d", arr, dl, pd)
	}
	var saved struct {
		Arrived    int64 `json:"arrived"`
		Downloaded int64 `json:"downloaded"`
		PiecesDone int   `json:"piecesDone"`
	}
	raw, err := os.ReadFile(filepath.Join(tasksDir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Arrived != 0 || saved.Downloaded != 0 || saved.PiecesDone != 0 {
		t.Fatalf("重新开始后落盘 JSON 仍残留旧账: arrived=%d downloaded=%d piecesDone=%d",
			saved.Arrived, saved.Downloaded, saved.PiecesDone)
	}
}

// TestBTRemoveRunningCleansData 删除运行中的 BT 任务:就地半成品按临时
// 数据清理(与取消同语义),无论是否勾选"删除文件"——deleteFile 只
// 管成品。旧实现只清旧布局路径,数据目录永久遗留成孤儿。
func TestBTRemoveRunningCleansData(t *testing.T) {
	m, id, folder, _, _ := btLifecycleEnv(t, 1)
	if v := waitForStatus(t, m, id, StatusRunning, 15*time.Second); v.Size == 0 {
		t.Fatalf("任务未运行: %+v", v)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(folder); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("会话未创建数据目录")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := m.Remove(id, false); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(8 * time.Second)
	for {
		if _, err := os.Stat(folder); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("删除运行中任务后数据目录未清理")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestBTChooseReturnsImmediately 改选接口不得同步等旧会话退出:会话收尾
// 里的 stopped 通告(5s 预算)会被黑洞/失效 tracker 拖满,旧实现把它整个
// 拉进 HTTP handler(实测 5028ms)。异步重启后接口立即返回,任务经
// paused→waiting→running 完成平滑换会话。
func TestBTChooseReturnsImmediately(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { time.Sleep(60 * time.Second); c.Close() }(conn)
		}
	}()

	m, id, _, _, meta := btLifecycleEnv(t, 4096) // 限速:保证改选落在 running 态
	if v := waitForStatus(t, m, id, StatusRunning, 15*time.Second); v.Size == 0 {
		t.Fatalf("任务未运行: %+v", v)
	}
	// 热挂黑洞 tracker:让当前会话的收尾必然被通告拖满预算
	if _, err := m.AddTaskTracker(id, "http://"+ln.Addr().String()+"/announce"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	if _, err := m.ChooseBTFiles(id, []string{"doc.txt"}, false); err != nil {
		t.Fatalf("改选失败: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("改选接口阻塞 %v(旧会话收尾应异步进行)", d)
	}

	// 异步重启最终仍须按新选择完成(旧会话退出→重新排队→新会话)
	wantSize := btFileLength(meta, "doc.txt")
	deadline := time.Now().Add(45 * time.Second)
	for {
		v := m.get(id).View()
		if v.Status == StatusCompleted || v.Status == StatusSeeding {
			if v.Size != wantSize || v.PiecesDone != v.PiecesTotal {
				t.Fatalf("异步重启后账目不自洽: size=%d (want %d) pieces=%d/%d",
					v.Size, wantSize, v.PiecesDone, v.PiecesTotal)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("异步重启未完成: %+v", m.get(id).View())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestBTRemovePausedCleansData 暂停态删除任务:半成品就地数据必须一并清理。
// 旧实现的非运行态删除只清 legacy 临时路径(.zdl.part/.zdlbt),就地布局的
// 半成品目录必遗留——记录已删,数据成了无法续传的无主垃圾(实测:暂停
// 后删除任务,数据目录原样留在下载目录里)。
func TestBTRemovePausedCleansData(t *testing.T) {
	m, id, folder, _, _ := btLifecycleEnv(t, 1) // 1 B/s:必然停在半成品
	if v := waitForStatus(t, m, id, StatusRunning, 15*time.Second); v.Size == 0 {
		t.Fatalf("任务未运行: %+v", v)
	}
	btWaitDir(t, folder)
	if err := m.Pause(id); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, id, StatusPaused, 10*time.Second)
	if _, err := os.Stat(folder); err != nil {
		t.Fatalf("暂停态数据目录应存在: %v", err)
	}

	if err := m.Remove(id, false); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(folder); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("暂停态删除后数据目录未清理")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestBTCancelChoosingSiblingKeepsActiveData 同名守护:磁力 choosing 任务
// 与据其种子创建的下载任务天然同名(Dir/Filename 相同)。取消/删除磁力
// 任务绝不能动下载任务的数据——旧实现照删,把运行中会话正写着的目录
// 整个 RemoveAll,已关闭文件被删、打开中的留下(实测残留恰好一个 .!zd),
// 已落盘数据也被误清。
func TestBTCancelChoosingSiblingKeepsActiveData(t *testing.T) {
	m, id, folder, _, meta := btLifecycleEnv(t, 4096)
	if v := waitForStatus(t, m, id, StatusRunning, 15*time.Second); v.Size == 0 {
		t.Fatalf("任务未运行: %+v", v)
	}
	// 等数据目录落成(会话启动即建目录)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(folder); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("运行任务的数据目录未创建")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 模拟磁力任务:元数据就绪后挂起 choosing,与运行任务同名同目录
	sib := m.newBTTask(CreateReq{Magnet: "magnet:?xt=urn:btih:" + fmt.Sprintf("%x", meta.InfoHash)}, filepath.Dir(folder))
	sib.mu.Lock()
	sib.Filename = filepath.Base(folder)
	sib.Status = StatusChoosing // choosing 不可被调度,不会真跑会话
	sib.mu.Unlock()
	m.registerBTTask(sib)

	if err := m.Cancel(sib.ID); err != nil {
		t.Fatalf("取消 choosing 兄弟任务失败: %v", err)
	}

	// 运行任务的数据目录必须原封不动,任务本身不受打扰
	if _, err := os.Stat(folder); err != nil {
		t.Fatal("取消同名 choosing 任务误删了运行任务的数据目录")
	}
	if v := m.get(id).View(); v.Status != StatusRunning {
		t.Fatalf("运行任务状态被扰动: %+v", v)
	}
}
