// fakeleech 模拟"只取不予"的 PCDN 吸血端:向 ZDownloader 的 BT 监听端口
// 发起明文连接,只请求数据、从不回传,并可选周期性发送 have 谎报进度(实测
// PCDN 伪装成 qBittorrent 4.6.7 正是这套行为)。用于本地端到端验证「自动
// 封禁PCDN客户端」的只取不予判据。
//
// 两种形态:
//   - 高级 PCDN(-fake=true,默认):零回传 + 假增进度,验证字节判据
//     (进度不可信,看真实字节互惠)。
//   - 低级 PCDN(-serveKB>0):声明持有部分分片、对 request 回传真数据
//     (需要 -data 指向种子对应的源文件)挣"互惠 peer"待遇,却从不发
//     have——进度纹丝不动,验证进度判据(-fake=false)。用于复现"会做
//     字节互惠但从不真正完成任何 piece"的吸血端。
//
// 用法(配合在被测实例上下调阈值,几十秒即可复现封禁):
//
//	ZD_LEECH_MIN_SERVED=262144  ZD_LEECH_MIN_ELAPSED=20s
//	go run ./scripts/fakeleech -torrent x.torrent -peer 127.0.0.1:40985
//	go run ./scripts/fakeleech -torrent x.torrent -peer ... -fake=false \
//	    -serveKB 64 -sliceFrom 60 -data x.bin
package main

import (
	"crypto/rand"
	"encoding/binary"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"time"

	"zdownloader/internal/bt"
)

const blockSize = 16384

// msg 组装一条 BT 线协议消息:4 字节长度前缀(含 id)+ id + 负载。
func msg(id byte, payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(out, uint32(1+len(payload)))
	out[4] = id
	copy(out[5:], payload)
	return out
}

func main() {
	torrentPath := flag.String("torrent", "", "种子文件(取 infohash 与分片表)")
	peerAddr := flag.String("peer", "127.0.0.1:40985", "目标 BT 监听地址(直连,省去 tracker)")
	idPrefix := flag.String("peerid", "-qB4670-", "peer id 前缀(默认伪装 qBittorrent 4.6.7)")
	fakeProgress := flag.Bool("fake", true, "周期性 have 谎报进度(PCDN 特征)")
	serveKB := flag.Int("serveKB", 0, "回传限速 KB/s(>0 = 互惠假种模式:声明持有分片并对 request 回传真数据)")
	dataPath := flag.String("data", "", "源文件路径(互惠假种模式必填,种子须为单文件种)")
	sliceFrom := flag.Int("sliceFrom", 0, "互惠假种模式声称持有的起始分片百分比(0=全部持有)")
	vName := flag.String("v", "", "扩展握手 v 字段自报客户端全名(真实客户端必发,可与 peerid 推断名不同形;复现右键封禁/黑名单的口径场景)")
	flag.Parse()
	if *torrentPath == "" {
		log.Fatal("缺少 -torrent")
	}
	raw, err := os.ReadFile(*torrentPath)
	if err != nil {
		log.Fatal(err)
	}
	meta, err := bt.ParseTorrent(raw)
	if err != nil {
		log.Fatal(err)
	}

	// 互惠假种模式:载入源文件,声明持有尾段分片,对 request 回传真数据
	var data []byte
	if *serveKB > 0 {
		if *sliceFrom <= 0 || *sliceFrom >= 100 {
			log.Fatal("互惠假种模式需要 0 < -sliceFrom < 100(声明部分持有,其余照常索取)")
		}
		if *dataPath == "" {
			log.Fatal("互惠假种模式需要 -data 指向种子对应的源文件")
		}
		if data, err = os.ReadFile(*dataPath); err != nil {
			log.Fatal(err)
		}
		log.Printf("互惠假种模式:声明持有 %d%% 起的分片,以 %dKB/s 回传真数据(源 %d 字节)",
			*sliceFrom, *serveKB, len(data))
	}

	conn, err := net.DialTimeout("tcp", *peerAddr, 5*time.Second)
	if err != nil {
		log.Fatal("连接失败: ", err)
	}
	defer conn.Close()

	// 明文握手(入站嗅探首字节 0x13 即判明文)
	hs := make([]byte, 0, 68)
	hs = append(hs, 19)
	hs = append(hs, "BitTorrent protocol"...)
	hs = append(hs, make([]byte, 8)...)
	hs = append(hs, meta.InfoHash[:]...)
	pid := []byte(*idPrefix)
	for len(pid) < 20 {
		var b [1]byte
		_, _ = rand.Read(b[:])
		pid = append(pid, 'a'+b[0]%26)
	}
	hs = append(hs, pid[:20]...)
	if _, err := conn.Write(hs); err != nil {
		log.Fatal("握手写失败: ", err)
	}
	resp := make([]byte, 68)
	if _, err := io.ReadFull(conn, resp); err != nil {
		log.Fatal("握手读失败: ", err)
	}
	if resp[0] != 19 || string(resp[1:20]) != "BitTorrent protocol" {
		log.Fatalf("对端不是 BT 握手: %q", resp[:20])
	}
	if string(resp[28:48]) != string(meta.InfoHash[:]) {
		log.Fatal("infohash 不匹配")
	}
	log.Printf("已连接 %s,infohash %x,分片 %d × %d 字节",
		*peerAddr, meta.InfoHash[:6], meta.PieceCount(), meta.PieceLength)

	pieces := meta.PieceCount()
	plen := int(meta.PieceLength)
	if plen <= 0 {
		plen = 256 << 10
	}
	blocksPer := (plen + blockSize - 1) / blockSize
	// 互惠假种只索取"自己没有"的头段分片,更像真客户端
	reqPieces := pieces
	if *serveKB > 0 {
		reqPieces = *sliceFrom * pieces / 100
		if reqPieces <= 0 || reqPieces >= pieces {
			log.Fatal("分片太少,切不出互惠假种的持有段")
		}
	}
	next := 0
	mkReq := func() []byte {
		b := next % (reqPieces * blocksPer)
		next++
		piece := b / blocksPer
		l := int(meta.PieceLen(piece))
		begin := (b % blocksPer) * blockSize
		if begin >= l {
			begin = 0
		}
		n := l - begin
		if n > blockSize {
			n = blockSize
		}
		p := make([]byte, 12)
		binary.BigEndian.PutUint32(p[0:], uint32(piece))
		binary.BigEndian.PutUint32(p[4:], uint32(begin))
		binary.BigEndian.PutUint32(p[8:], uint32(n))
		return msg(6, p)
	}

	// bitfield(空 = 0 分片下载方;互惠假种 = 声明持有尾段) + interested
	if *serveKB > 0 {
		bf := make([]byte, (pieces+7)/8)
		for i := reqPieces; i < pieces; i++ {
			bf[i/8] |= 1 << (7 - uint(i%8))
		}
		conn.Write(msg(5, bf))
	} else {
		conn.Write(msg(5, make([]byte, (pieces+7)/8)))
	}
	conn.Write(msg(2, nil))
	// 扩展握手(id 20,id 字典 0):v 字段自报全名,复现真实客户端形态
	if *vName != "" {
		conn.Write(msg(20, append([]byte{bt.ExtHandshakeID}, bt.BuildExtHandshake(0, 0, *vName)...)))
	}

	var got, sentMsg int64
	var servedBytes int64
	start := time.Now()
	stop := make(chan struct{})

	// 互惠假种:对 request 回传真数据。令牌桶按"自首次回传起的累计配额"
	// 放行,把回传拖在限速内(读循环单协程,睡在这里无竞态)
	var rateAnchor time.Time
	serveBlock := func(idx, begin, n uint32) {
		off := int64(idx)*int64(plen) + int64(begin)
		if n == 0 || off < 0 || off+int64(n) > int64(len(data)) {
			return
		}
		rateBps := int64(*serveKB) << 10
		if rateAnchor.IsZero() {
			rateAnchor = time.Now()
		}
		if quota := int64(time.Since(rateAnchor).Seconds() * float64(rateBps)); servedBytes+int64(n) > quota {
			time.Sleep(time.Duration(float64(servedBytes+int64(n)-quota) / float64(rateBps) * float64(time.Second)))
		}
		p := make([]byte, 8+int(n))
		binary.BigEndian.PutUint32(p[0:4], idx)
		binary.BigEndian.PutUint32(p[4:8], begin)
		copy(p[8:], data[off:off+int64(n)])
		conn.Write(msg(7, p))
		servedBytes += int64(n)
	}
	go func() { // 每 5 秒报一次吸血量
		tk := time.NewTicker(5 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				log.Printf("已运行 %s:从对端下载 %.2f MB(回传 %.2f MB)",
					time.Since(start).Round(time.Second), float64(got)/(1<<20), float64(servedBytes)/(1<<20))
			}
		}
	}()

	// 假进度:周期性 have 谎报"我又拿到一片"
	if *fakeProgress {
		go func() {
			tk := time.NewTicker(2 * time.Second)
			defer tk.Stop()
			claim := 0
			for {
				select {
				case <-stop:
					return
				case <-tk.C:
					if claim >= pieces {
						return
					}
					p := make([]byte, 4)
					binary.BigEndian.PutUint32(p, uint32(claim))
					claim++
					conn.Write(msg(4, p))
				}
			}
		}()
	}

	// 起始灌满管线,之后每收到一块/被拒一块就补一条
	outstanding := 0
	for outstanding < 64 {
		conn.Write(mkReq())
		outstanding++
		sentMsg++
	}
	hdr := make([]byte, 4)
	var unchokedPeer bool
	for {
		if _, err := io.ReadFull(conn, hdr); err != nil {
			close(stop)
			log.Printf("连接结束(%v):累计下载 %.2f MB / 回传 %.2f MB / 约 %s——若刚过阈值,即为封禁生效",
				err, float64(got)/(1<<20), float64(servedBytes)/(1<<20), time.Since(start).Round(time.Second))
			return
		}
		n := binary.BigEndian.Uint32(hdr)
		if n == 0 || n > 1<<20 {
			continue
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(conn, body); err != nil {
			close(stop)
			log.Printf("连接结束(%v):累计下载 %.2f MB / 回传 %.2f MB",
				err, float64(got)/(1<<20), float64(servedBytes)/(1<<20))
			return
		}
		switch body[0] {
		case 1: // unchoke:立刻灌满管线
			for outstanding < 64 {
				conn.Write(mkReq())
				outstanding++
			}
		case 2: // interested:互惠假种立即放行,让对方来取数据
			if *serveKB > 0 && !unchokedPeer {
				unchokedPeer = true
				conn.Write(msg(1, nil))
			}
		case 6: // request:互惠假种回传真数据
			if *serveKB > 0 && len(body) >= 13 {
				serveBlock(binary.BigEndian.Uint32(body[1:5]),
					binary.BigEndian.Uint32(body[5:9]),
					binary.BigEndian.Uint32(body[9:13]))
			}
		case 7: // piece:收下丢弃,补一条新请求
			got += int64(len(body) - 9)
			outstanding--
			conn.Write(mkReq())
		case 16: // reject request(fast 扩展):同样补一条
			outstanding--
			conn.Write(mkReq())
		}
	}
}
