package engine

// 边下边播元数据探测:容器解析器纯函数单测(合成 MP4/MKV 字节流)+
// 引擎编排集成测试(真实做种端:探测精化 pin、码率自适应、剧集预热)。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zdownloader/internal/bt"
)

// ---------- 合成 MP4 ----------

func mp4Atom(typ string, body []byte) []byte {
	out := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(out)))
	copy(out[4:8], typ)
	copy(out[8:], body)
	return out
}

// mp4Mvhd 合成 mvhd atom(填充至常见尺寸);v1 用 64 位时长字段。
func mp4Mvhd(seconds float64, v1 bool) []byte {
	body := make([]byte, 108)
	ts := uint32(1000)
	if v1 {
		body[0] = 1
		binary.BigEndian.PutUint32(body[20:24], ts)
		binary.BigEndian.PutUint64(body[24:32], uint64(seconds*float64(ts)))
	} else {
		binary.BigEndian.PutUint32(body[12:16], ts)
		binary.BigEndian.PutUint32(body[16:20], uint32(seconds*float64(ts)))
	}
	return mp4Atom("mvhd", body)
}

func TestMP4WalkHead(t *testing.T) {
	ftyp := mp4Atom("ftyp", bytes.Repeat([]byte{1}, 16)) // 24B
	moov := mp4Atom("moov", append(mp4Mvhd(100, false), bytes.Repeat([]byte{2}, 64)...))
	mdat := mp4Atom("mdat", make([]byte, 4096))

	// faststart:ftyp→moov→mdat
	file := append(append(append([]byte{}, ftyp...), moov...), mdat...)
	r, ok := mp4WalkHead(file, int64(len(file)))
	if !ok || !r.moovFront {
		t.Fatalf("faststart 应识别,ok=%v %+v", ok, r)
	}
	if r.moovOff != int64(len(ftyp)) || r.moovEnd != int64(len(ftyp)+len(moov)) || r.headEnd != int64(len(ftyp)+len(moov)) {
		t.Fatalf("faststart moov 区间不符: %+v", r)
	}
	if r.mdatEnd != int64(len(file)) {
		t.Fatalf("mdatEnd 应为文件尾,得到 %d", r.mdatEnd)
	}
	if s, ok := mp4MoovSeconds(file[len(ftyp):]); !ok || math.Abs(s-100) > 0.001 {
		t.Fatalf("mvhd 时长应为 100s,得到 %v ok=%v", s, ok)
	}

	// moov 在尾:ftyp→mdat(大)→moov(头缓冲只够走到 mdat 头)
	moov2 := mp4Atom("moov", append(mp4Mvhd(200, false), bytes.Repeat([]byte{3}, 128<<10)...))
	file2 := append(append(append([]byte{}, ftyp...),
		mp4Atom("mdat", make([]byte, 512<<10))...), moov2...)
	head2 := file2[:64<<10]
	r2, ok2 := mp4WalkHead(head2, int64(len(file2)))
	if !ok2 || r2.moovFront {
		t.Fatalf("moov 在尾应识别,ok=%v %+v", ok2, r2)
	}
	if r2.mdatEnd != int64(len(ftyp)+8+512<<10) {
		t.Fatalf("mdatEnd 应为 mdat 结束偏移,得到 %d", r2.mdatEnd)
	}
	if r2.headEnd != int64(len(ftyp)) {
		t.Fatalf("headEnd 应为 mdat 起点偏移,得到 %d", r2.headEnd)
	}
	if s, ok := mp4MoovSeconds(file2[r2.mdatEnd:]); !ok || math.Abs(s-200) > 0.001 {
		t.Fatalf("moov 在尾 mvhd 时长应为 200s,得到 %v ok=%v", s, ok)
	}

	// 64 位扩展尺寸 mdat
	hdr := make([]byte, 16)
	binary.BigEndian.PutUint32(hdr[0:4], 1)
	copy(hdr[4:8], "mdat")
	binary.BigEndian.PutUint64(hdr[8:16], 16+1024)
	file3 := append(append([]byte{}, ftyp...), append(hdr, make([]byte, 1024)...)...)
	r3, ok3 := mp4WalkHead(file3, int64(len(file3)))
	if !ok3 || r3.mdatEnd != int64(len(file3)) {
		t.Fatalf("64 位 mdat 尺寸应解析,ok=%v mdatEnd=%d", ok3, r3.mdatEnd)
	}

	// 非法/垃圾头:ok=false(维持默认 pin)
	if _, ok4 := mp4WalkHead(bytes.Repeat([]byte{0x55}, 4096), 4096); ok4 {
		t.Fatal("垃圾头不应解析成功")
	}
}

func TestMP4MoovSecondsV1(t *testing.T) {
	moov := mp4Atom("moov", append(
		mp4Atom("free", make([]byte, 32)), // mvhd 非首子 atom 也可跳到
		mp4Mvhd(1234.5, true)...))
	s, ok := mp4MoovSeconds(moov)
	if !ok || math.Abs(s-1234.5) > 0.001 {
		t.Fatalf("v1 时长应为 1234.5s,得到 %v ok=%v", s, ok)
	}
}

// ---------- 合成 MKV ----------

func mkvIDBytes(id uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], id)
	i := 0
	for i < 7 && b[i] == 0 {
		i++
	}
	return bytes.Clone(b[i:])
}

func mkvSizeVint(v uint64) []byte {
	for n := 1; n <= 8; n++ {
		if v>>uint(7*n) == 0 { // v < 2^(7n):n 字节可容纳
			out := make([]byte, n)
			for i := 0; i < n; i++ {
				out[i] = byte(v >> uint(8*(n-1-i)))
			}
			out[0] |= byte(0x80 >> (n - 1))
			return out
		}
	}
	return nil
}

func mkvEl(id uint64, body []byte) []byte {
	out := append(mkvIDBytes(id), mkvSizeVint(uint64(len(body)))...)
	return append(out, body...)
}

func beU32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

func beF32(v float32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], math.Float32bits(v))
	return b[:]
}

func TestMKVWalkHead(t *testing.T) {
	ebml := mkvEl(mkvIDEbmlHeader, []byte{0x42, 0x86, 0x81, 0x01})
	seek := mkvEl(mkvIDSeekHead, make([]byte, 48))
	scale := mkvEl(mkvIDTimeScale, beU32(1000000)) // 默认 1ms 单位
	dur := mkvEl(mkvIDDuration, beF32(100000))     // 100000 单位 = 100s
	info := mkvEl(mkvIDInfo, append(append([]byte{}, scale...), dur...))
	tracks := mkvEl(mkvIDTracks, make([]byte, 64))
	cluster := mkvEl(mkvIDCluster, make([]byte, 128))
	segBody := concat(seek, info, tracks, cluster)
	segHdr := append(mkvIDBytes(mkvIDSegment), mkvSizeVint(uint64(len(segBody)))...)
	file := concat(ebml, segHdr, segBody)

	wantHeadEnd := int64(len(ebml) + len(segHdr) + len(seek) + len(info) + len(tracks))
	r := mkvWalkHead(file, int64(len(file)))
	if r.headEnd != wantHeadEnd {
		t.Fatalf("headEnd 应为首个 Cluster 起点 %d,得到 %d", wantHeadEnd, r.headEnd)
	}
	if math.Abs(r.durSec-100) > 0.001 {
		t.Fatalf("时长应为 100s,得到 %v", r.durSec)
	}

	// 自定义 TimecodeScale:500000ns × 200000 单位 = 100s
	info2 := mkvEl(mkvIDInfo, concat(mkvEl(mkvIDTimeScale, beU32(500000)), mkvEl(mkvIDDuration, beF32(200000))))
	segBody2 := concat(seek, info2, tracks, cluster)
	seg2 := mkvEl(mkvIDSegment, segBody2)
	file2 := concat(ebml, seg2)
	r2 := mkvWalkHead(file2, int64(len(file2)))
	if math.Abs(r2.durSec-100) > 0.001 {
		t.Fatalf("自定义 scale 时长应为 100s,得到 %v", r2.durSec)
	}
	if r2.headEnd == 0 {
		t.Fatal("headEnd 不应缺失")
	}
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestMKVCuesOffset(t *testing.T) {
	junk := bytes.Repeat([]byte{0xAB}, 64<<10)
	cues := mkvEl(mkvIDCues, make([]byte, 4096))
	trailer := make([]byte, 16)
	tail := concat(junk, cues, trailer)
	if off := mkvCuesOffset(tail, 0, int64(len(tail))); off != int64(len(junk)) {
		t.Fatalf("应定位 Cues 于 %d,得到 %d", len(junk), off)
	}
	// 尾区不在缓冲起点:tailStart 偏移换算
	if off := mkvCuesOffset(tail[len(junk):], int64(len(junk)), int64(len(tail))); off != int64(len(junk)) {
		t.Fatalf("带 tailStart 的定位应一致,得到 %d", off)
	}
	// 纯负载:未命中
	if off := mkvCuesOffset(junk, 0, int64(len(junk))); off != -1 {
		t.Fatalf("无 Cues 应返回 -1,得到 %d", off)
	}
	// 假阳性:ID 模式后随尺寸与文件尾不吻合(视频负载随机含 ID 字节)
	fp := concat(bytes.Repeat([]byte{0xAB}, 100),
		[]byte{0x1C, 0x53, 0xBB, 0x6C}, mkvSizeVint(5000), make([]byte, 64))
	if off := mkvCuesOffset(fp, 0, int64(len(fp))); off != -1 {
		t.Fatalf("尺寸不吻合的 ID 匹配应排除,得到 %d", off)
	}
}

// ---------- 集成:探测精化 pin + 码率自适应 ----------

// buildMP4MoovAtEnd 合成 moov 在尾的 MP4(ftyp→mdat→moov,moov > 1MB):
// 默认 1MB 尾 pin 覆盖不全,探测必须把尾 pin 扩到 moov 起点才能拿到完整索引。
func buildMP4MoovAtEnd() (content []byte, mdatEnd int64) {
	ftyp := mp4Atom("ftyp", bytes.Repeat([]byte{1}, 16)) // 24B
	mdat := mp4Atom("mdat", make([]byte, 512<<10))       // 8 + 512KB
	base := int64(len(ftyp) + len(mdat))                 // moov 起点(= mdatEnd)
	// 最小视频轨(stts/stsc/stco):供探测构建“时间→字节”定位表
	trak := mp4MkTrak("vide", 1000,
		mp4Stts(2, 500), // 3 chunk × 1 样本 × 0.5s
		mp4Stsc(1, 1),
		mp4Stco(uint32(base), uint32(base+500), uint32(base+1000)))
	moovBody := append(append(mp4Mvhd(200, false), trak...), bytes.Repeat([]byte{7}, (1<<20)+(400<<10))...)
	moov := mp4Atom("moov", moovBody) // ≈ 1.4MB
	content = concat(ftyp, mdat, moov)
	return content, base
}

func TestBTStreamMetaProbe(t *testing.T) {
	tracker := fakeTracker(t)
	content, mdatEnd := buildMP4MoovAtEnd()
	dir := streamTempDir(t)
	srcFile := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(srcFile, content, 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := bt.BuildTorrentFromDisk(srcFile, "", "", 256<<10) // 256KB piece
	if err != nil {
		t.Fatal(err)
	}
	torrent, err := bt.BuildTorrentBytes(meta, tracker.URL+"/announce", "")
	if err != nil {
		t.Fatal(err)
	}
	if meta, err = bt.ParseTorrent(torrent); err != nil {
		t.Fatal(err)
	}
	seed, err := bt.NewSeedClient(meta, srcFile, bt.ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.SetLimits(0, 256*1024)
	seed.Start()
	defer seed.Close()

	dl := streamTempDir(t)
	m := NewManager(testConf(dl), filepath.Join(t.TempDir(), "cfg.json"), filepath.Join(t.TempDir(), "tasks"))
	ctx, cancel := context.WithCancel(context.Background())
	defer stopManager(cancel, m)
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	views, errs := m.AddBT(CreateReq{TorrentB64: base64.StdEncoding.EncodeToString(torrent)})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID

	deadline := time.Now().Add(20 * time.Second)
	var info *BTStreamInfo
	for time.Now().Before(deadline) {
		if info, err = m.BTStreamOpen(id, "video.mp4", 0); err == nil {
			break
		}
		if !errors.Is(err, ErrStreamNoSession) {
			t.Fatalf("BTStreamOpen: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("BTStreamOpen 重试超时: %v", err)
	}
	pl := info.PieceLen
	wantTailFirst := int(mdatEnd / pl)
	wantTailLast := int((int64(len(content)) - 1) / pl)
	wantAhead := streamAheadFor(float64(len(content))/200.0, pl)

	// 轮询等探测结论落账:头 pin 精化到 mdat 起点、尾 pin 扩到 moov 起点
	//(默认尾 pin 只覆盖最后 1MB,piece 数明显更少)、码率与自适应窗口生效
	var st *BTStreamStatus
	for time.Now().Before(deadline) {
		if st, err = m.BTStreamStatus(id); err != nil {
			t.Fatal(err)
		}
		if st.PinHeadFirst == 0 && st.PinHeadLast == 0 && st.PinTailFirst == wantTailFirst &&
			st.Bitrate > 0 && st.Ahead == wantAhead && st.MetaEnd == 24 &&
			len(st.Seek) >= 3 && st.Seek[0][1] == float64(mdatEnd) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	// 内容起点 = mdat 起点:本样本 ftyp 24B 后即 mdat(moov 在尾)
	if st.MetaEnd != 24 {
		t.Fatalf("内容起点应为 mdat 起点 24(播放头内插锚),得到 %d", st.MetaEnd)
	}
	if st == nil || st.PinTailFirst != wantTailFirst || st.PinTailLast != wantTailLast {
		t.Fatalf("尾 pin 应精化到 [%d,%d](moov 起点),得到 [%d,%d]",
			wantTailFirst, wantTailLast, st.PinTailFirst, st.PinTailLast)
	}
	if st.PinHeadLast != 0 {
		t.Fatalf("头 pin 应精化到 piece 0(mdat 起点),得到 %d", st.PinHeadLast)
	}
	// 序列化回归:pinHeadFirst=0 不得被 omitempty 丢弃——单文件种子的头部
	// pin 恒自 piece 0 起,零值字段被丢前端就永远画不出头部 pin(实测教训)
	if jb, jerr := json.Marshal(st); jerr != nil || !strings.Contains(string(jb), `"pinHeadFirst":0`) {
		t.Fatalf("status JSON 应包含 pinHeadFirst:0,err=%v", jerr)
	}
	if st.Bitrate <= 0 || math.Abs(st.Bitrate-float64(len(content))/200) > float64(len(content))/200*0.02 {
		t.Fatalf("码率应 ≈ %d/200 B/s,得到 %v", len(content), st.Bitrate)
	}
	if st.Ahead != wantAhead || wantAhead >= 64 {
		t.Fatalf("自适应窗口应为 %d(与初始 64 可区分),得到 %d", wantAhead, st.Ahead)
	}
	// 收尾:停做种防 Windows 句柄竞态
	m.BTStreamStop(id)
	if v, _ := m.Get(id); v.Status == StatusSeeding {
		if err := m.StopSeeding(id); err != nil {
			t.Fatal(err)
		}
		waitForStatus(t, m, id, StatusCompleted, 10*time.Second)
	}
}

// ---------- 集成:剧集预热 ----------

func TestBTStreamWarm(t *testing.T) {
	tracker := fakeTracker(t)
	dir := streamTempDir(t)
	srcDir := filepath.Join(dir, "pack")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, n int, seed byte) {
		b := make([]byte, n)
		for i := range b {
			b[i] = seed
		}
		if err := os.WriteFile(filepath.Join(srcDir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("ep1.mp4", 1<<20, 1)
	mk("ep2.mp4", 1<<20, 2)
	_, meta, err := bt.BuildTorrentFromDisk(srcDir, "", "", 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	torrent, err := bt.BuildTorrentBytes(meta, tracker.URL+"/announce", "")
	if err != nil {
		t.Fatal(err)
	}
	if meta, err = bt.ParseTorrent(torrent); err != nil {
		t.Fatal(err)
	}
	seed, err := bt.NewSeedClient(meta, srcDir, bt.ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.SetLimits(0, 128*1024) // 限速保持下载中
	seed.Start()
	defer seed.Close()

	dl := streamTempDir(t)
	m := NewManager(testConf(dl), filepath.Join(streamTempDir(t), "cfg.json"), filepath.Join(streamTempDir(t), "tasks"))
	ctx, cancel := context.WithCancel(context.Background())
	defer stopManager(cancel, m)
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	views, errs := m.AddBT(CreateReq{TorrentB64: base64.StdEncoding.EncodeToString(torrent)})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err = m.BTStreamOpen(id, "ep1.mp4", 0); err == nil {
			break
		}
		if !errors.Is(err, ErrStreamNoSession) {
			t.Fatalf("BTStreamOpen: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("BTStreamOpen 重试超时: %v", err)
	}
	// ep2 首件下标:ep1 = 1MB = 64 piece
	ep2First := int(meta.Files[1].Offset / meta.PieceLength)
	if ep2First != 64 {
		t.Fatalf("测试前提:ep2 应自 piece 64 开始,得到 %d", ep2First)
	}
	st, err := m.BTStreamStatus(id)
	if err != nil {
		t.Fatal(err)
	}
	if st.PinHeadLast >= ep2First {
		t.Fatalf("预热前 pin 不应覆盖 ep2(piece %d),得到 PinHeadLast=%d", ep2First, st.PinHeadLast)
	}
	// 预热 ep2:头/尾 piece 并入 pin;幂等重放
	if err := m.BTStreamWarm(id, "ep2.mp4"); err != nil {
		t.Fatalf("BTStreamWarm: %v", err)
	}
	if err := m.BTStreamWarm(id, "ep2.mp4"); err != nil {
		t.Fatalf("BTStreamWarm 幂等重放: %v", err)
	}
	st, err = m.BTStreamStatus(id)
	if err != nil {
		t.Fatal(err)
	}
	// 展示口径:pin 限制在当前文件(ep1)区间内——预热的 ep2 piece 在区间外,
	// 若混入"首~尾"夹逼画带会把整个进度条涂黄(实测反馈)
	if st.PinHeadLast >= ep2First {
		t.Fatalf("预热不应把 pin 展示撑到 ep2(piece %d),得到 PinHeadLast=%d", ep2First, st.PinHeadLast)
	}
	// picker 实际 pin 已并入 ep2 头部(piece ≥ ep2First):预热优先级生效
	if tt := m.get(id); tt != nil {
		tt.mu.Lock()
		dl := tt.btcl
		tt.mu.Unlock()
		if dl != nil {
			_, _, _, ph, _, _, _ := dl.StreamState()
			covered := false
			for _, p := range ph {
				if p >= ep2First {
					covered = true
					break
				}
			}
			if !covered {
				t.Fatalf("预热后 picker pin 应覆盖 ep2 头部(piece ≥%d),pinHead=%v", ep2First, ph)
			}
		}
	}
	// 预热状态:触发即 Warming;预热 piece 全部落定后 WarmReady(黄点→绿点)
	if !st.Warming {
		t.Fatal("预热后 status.Warming 应为 true")
	}
	ready := false
	warmDL := time.Now().Add(25 * time.Second)
	for time.Now().Before(warmDL) {
		if st, err = m.BTStreamStatus(id); err != nil {
			t.Fatal(err)
		}
		if st.WarmReady {
			ready = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		t.Fatal("预热的 piece 应最终全部落定(WarmReady)")
	}
	// 校验性错误:不存在的文件
	if err := m.BTStreamWarm(id, "ep3.mp4"); err == nil {
		t.Fatal("不存在的文件预热应报错")
	}
	m.BTStreamStop(id)
	if v, _ := m.Get(id); v.Status == StatusSeeding {
		if err := m.StopSeeding(id); err != nil {
			t.Fatal(err)
		}
		waitForStatus(t, m, id, StatusCompleted, 10*time.Second)
	}
}

// TestBTStreamEpochGuard 开流代次门:快速切集后迟到的旧代次请求(被放弃
// 的播放器资源重试)必须被拒,流会话不被拽回旧文件(位图/游标横跳的根治);
// 不带代次(外部播放器)保持自由切换语义。
func TestBTStreamEpochGuard(t *testing.T) {
	tracker := fakeTracker(t)
	dir := streamTempDir(t)
	srcDir := filepath.Join(dir, "pack")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"ep1.mp4", "ep2.mp4"} {
		b := make([]byte, 512<<10)
		b[0] = byte(i + 1)
		if err := os.WriteFile(filepath.Join(srcDir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, meta, err := bt.BuildTorrentFromDisk(srcDir, "", "", 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	torrent, err := bt.BuildTorrentBytes(meta, tracker.URL+"/announce", "")
	if err != nil {
		t.Fatal(err)
	}
	if meta, err = bt.ParseTorrent(torrent); err != nil {
		t.Fatal(err)
	}
	seed, err := bt.NewSeedClient(meta, srcDir, bt.ClientConfig{MaxPeers: 8})
	if err != nil {
		t.Fatal(err)
	}
	seed.SetLimits(0, 64*1024)
	seed.Start()
	defer seed.Close()

	dl := streamTempDir(t)
	m := NewManager(testConf(dl), filepath.Join(streamTempDir(t), "cfg.json"), filepath.Join(streamTempDir(t), "tasks"))
	ctx, cancel := context.WithCancel(context.Background())
	defer stopManager(cancel, m)
	go m.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	views, errs := m.AddBT(CreateReq{TorrentB64: base64.StdEncoding.EncodeToString(torrent)})
	if len(errs) > 0 {
		t.Fatalf("创建失败: %v", errs)
	}
	id := views[0].ID

	open := func(file string, epoch int64) error {
		var err error
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err = m.BTStreamOpen(id, file, epoch); err == nil {
				return nil
			}
			if !errors.Is(err, ErrStreamNoSession) {
				return err
			}
			time.Sleep(100 * time.Millisecond)
		}
		return err
	}
	curFile := func() string {
		st, err := m.BTStreamStatus(id)
		if err != nil {
			t.Fatal(err)
		}
		return st.File
	}

	if err := open("ep1.mp4", 1); err != nil {
		t.Fatalf("代次 1 打开 ep1: %v", err)
	}
	if err := open("ep2.mp4", 2); err != nil {
		t.Fatalf("代次 2 打开 ep2: %v", err)
	}
	if got := curFile(); got != "ep2.mp4" {
		t.Fatalf("当前会话应为 ep2,得到 %s", got)
	}
	// 迟到的旧代次请求(ep1 的播放器资源重试):拒绝,会话不动
	if _, err := m.BTStreamOpen(id, "ep1.mp4", 1); !errors.Is(err, ErrStreamStale) {
		t.Fatalf("旧代次应报 ErrStreamStale,得到 %v", err)
	}
	if got := curFile(); got != "ep2.mp4" {
		t.Fatalf("旧代次被拒后会话应保持 ep2,得到 %s", got)
	}
	// 同代次重试(ep2 播放器断线重连):放行,幂等
	if err := open("ep2.mp4", 2); err != nil {
		t.Fatalf("同代次重连 ep2: %v", err)
	}
	if got := curFile(); got != "ep2.mp4" {
		t.Fatalf("重连后会话应为 ep2,得到 %s", got)
	}
	// 不带代次(外部播放器,如 VLC 打开复制的 URL):自由切换
	if _, err := m.BTStreamOpen(id, "ep1.mp4", 0); err != nil {
		t.Fatalf("无代次(外部播放器)应可切换: %v", err)
	}
	if got := curFile(); got != "ep1.mp4" {
		t.Fatalf("无代次切换后应为 ep1,得到 %s", got)
	}
	m.BTStreamStop(id)
	if v, _ := m.Get(id); v.Status == StatusSeeding {
		if err := m.StopSeeding(id); err != nil {
			t.Fatal(err)
		}
		waitForStatus(t, m, id, StatusCompleted, 10*time.Second)
	}
}

// ---------- 时间→字节定位表 ----------

func be4(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

func mp4MkTrak(handler string, ts uint32, sttsBody, stscBody, stcoBody []byte) []byte {
	mdhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mdhd[12:16], ts)
	hdlr := make([]byte, 24)
	copy(hdlr[8:12], handler)
	stbl := concat(mp4Atom("stts", sttsBody), mp4Atom("stsc", stscBody), mp4Atom("stco", stcoBody))
	mdia := mp4Atom("mdia", concat(mp4Atom("mdhd", mdhd), mp4Atom("hdlr", hdlr), mp4Atom("minf", mp4Atom("stbl", stbl))))
	return mp4Atom("trak", mdia)
}

func mp4Stts(entries ...uint32) []byte { // (count, delta) 对
	b := concat(be4(0), be4(uint32(len(entries)/2)))
	for _, v := range entries {
		b = concat(b, be4(v))
	}
	return b
}

func mp4Stsc(first, n uint32) []byte {
	return concat(be4(0), be4(1), be4(first), be4(n), be4(1))
}

func mp4Stco(offs ...uint32) []byte {
	b := concat(be4(0), be4(uint32(len(offs))))
	for _, v := range offs {
		b = concat(b, be4(v))
	}
	return b
}

func TestMP4SeekPoints(t *testing.T) {
	// 音轨在前、视频轨在后:应选视频轨(其 stco 偏移 1000/2000/3000)
	video := mp4MkTrak("vide", 1000,
		mp4Stts(2, 500), // 2 样本 × 0.5s
		mp4Stsc(1, 1),   // 每 chunk 1 样本
		mp4Stco(1000, 2000, 3000))
	audio := mp4MkTrak("soun", 48000,
		mp4Stts(1, 1000), mp4Stsc(1, 1), mp4Stco(9000, 9500, 9900))
	moovBody := concat(mp4Mvhd(100, false), audio, video)
	pts := mp4BuildSeekPoints(moovBody)
	if len(pts) != 3 {
		t.Fatalf("应得 3 个点,得到 %d(%v)", len(pts), pts)
	}
	want := []seekPoint{{0, 1000}, {0.5, 2000}, {1.0, 3000}}
	for i, w := range want {
		if math.Abs(pts[i].t-w.t) > 0.001 || pts[i].off != w.off {
			t.Fatalf("点 %d 应为 %+v,得到 %+v", i, w, pts[i])
		}
	}
}

func TestMKVSeekPoints(t *testing.T) {
	cue := func(tm, pos uint64) []byte {
		return mkvEl(0xBB, concat(
			mkvEl(0xB3, beU64test(tm)),
			mkvEl(0xE7, mkvEl(0xF1, beU64test(pos)))))
	}
	cues := concat(cue(0, 100), cue(1000, 5000)) // 1000 × 1e6ns = 1s
	pts := mkvBuildSeekPoints(cues, 50, 1e6)
	if len(pts) != 2 {
		t.Fatalf("应得 2 个点,得到 %d", len(pts))
	}
	if pts[0].t != 0 || pts[0].off != 150 || math.Abs(pts[1].t-1) > 1e-9 || pts[1].off != 5050 {
		t.Fatalf("点位不符: %+v", pts)
	}
}

func beU64test(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}
