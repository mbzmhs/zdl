package engine

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 搭一个迷你 HLS 服务器:媒体清单 + 分片(可选 AES-128)。
type hlsTestServer struct {
	srv      *httptest.Server
	segments [][]byte
	key      []byte
	iv       []byte
}

func newHLSTestServer(t *testing.T, segments [][]byte, key []byte, iv []byte) *hlsTestServer {
	t.Helper()
	h := &hlsTestServer{segments: segments, key: key, iv: iv}
	mux := http.NewServeMux()
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(h.playlist()))
	})
	mux.HandleFunc("/key.bin", func(w http.ResponseWriter, r *http.Request) {
		w.Write(h.key)
	})
	for i := range segments {
		idx := i
		mux.HandleFunc(fmt.Sprintf("/seg%d.ts", idx), func(w http.ResponseWriter, r *http.Request) {
			data := segments[idx]
			if h.key != nil {
				data = encryptAES(data, h.key, h.ivFor(idx))
			}
			w.Write(data)
		})
	}
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hlsTestServer) ivFor(idx int) []byte {
	if h.iv != nil {
		return h.iv
	}
	iv := make([]byte, 16)
	iv[15] = byte(idx) // MEDIA-SEQUENCE=0 + index
	return iv
}

func (h *hlsTestServer) playlist() string {
	var b bytes.Buffer
	if h.key != nil {
		b.WriteString("#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\",IV=0x00000000000000000000000000000000\n")
		if h.iv == nil {
			// 无显式 IV 时由 media sequence 推导
			b.Reset()
			b.WriteString("#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n")
		}
	}
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:0\n")
	for i := range h.segments {
		b.WriteString(fmt.Sprintf("#EXTINF:9.0,\n/seg%d.ts\n", i))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func encryptAES(data []byte, key, iv []byte) []byte {
	// PKCS7 填充
	pad := aes.BlockSize - len(data)%aes.BlockSize
	buf := make([]byte, len(data)+pad)
	copy(buf, data)
	for i := len(data); i < len(buf); i++ {
		buf[i] = byte(pad)
	}
	block, _ := aes.NewCipher(key)
	mode := cipher.NewCBCEncrypter(block, iv)
	mode.CryptBlocks(buf, buf)
	return buf
}

func TestHLSDownloadPlain(t *testing.T) {
	segs := [][]byte{bigPayload(300000), bigPayload(150000), bytes.Repeat([]byte("Z"), 7777)}
	h := newHLSTestServer(t, segs, nil, nil)
	m := newTestManager(t, nil)

	views, _ := m.Create(CreateReq{URL: h.srv.URL + "/index.m3u8", Connections: 3})
	v := waitStatus(t, m, views[0].ID, StatusCompleted, 20*time.Second)
	if v.Kind != "hls" {
		t.Fatalf("任务类型应为 hls,实际 %s", v.Kind)
	}
	if v.SegDone != len(segs) || v.SegTotal != len(segs) {
		t.Fatalf("分片进度 %d/%d", v.SegDone, v.SegTotal)
	}
	got, err := os.ReadFile(filepath.Join(v.Dir, v.Filename))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.Join(segs, nil)) {
		t.Fatalf("TS 合并内容不一致: %d 字节", len(got))
	}
}

func TestHLSDownloadEncrypted(t *testing.T) {
	key := []byte("0123456789abcdef")
	segs := [][]byte{bigPayload(200000), bytes.Repeat([]byte{7}, 12345)}
	h := newHLSTestServer(t, segs, key, nil) // 无显式 IV → 由序号推导
	m := newTestManager(t, nil)

	views, _ := m.Create(CreateReq{URL: h.srv.URL + "/index.m3u8"})
	v := waitStatus(t, m, views[0].ID, StatusCompleted, 20*time.Second)
	if !v.Encrypted {
		t.Fatal("任务应标记 Encrypted")
	}
	got, _ := os.ReadFile(filepath.Join(v.Dir, v.Filename))
	if !bytes.Equal(got, bytes.Join(segs, nil)) {
		t.Fatal("解密后内容不一致")
	}
}

func TestHLSMasterPlaylistPicksBest(t *testing.T) {
	seg := bigPayload(100000)
	h := newHLSTestServer(t, [][]byte{seg}, nil, nil)
	// 主清单指向同一个 index.m3u8(两个变体)
	mux := h.srv.Config.Handler.(*http.ServeMux)
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nindex.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2400000\nindex.m3u8\n")
	})
	m := newTestManager(t, nil)
	views, _ := m.Create(CreateReq{URL: h.srv.URL + "/master.m3u8"})
	v := waitStatus(t, m, views[0].ID, StatusCompleted, 20*time.Second)
	got, _ := os.ReadFile(filepath.Join(v.Dir, v.Filename))
	if !bytes.Equal(got, seg) {
		t.Fatal("主清单选型后内容不一致")
	}
}

func TestHLSLiveRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.0,\n/seg0.ts\n"))
	}))
	t.Cleanup(srv.Close)
	m := newTestManager(t, nil)
	views, _ := m.Create(CreateReq{URL: srv.URL + "/live.m3u8"})
	v := waitStatus(t, m, views[0].ID, StatusError, 10*time.Second)
	if v.Err == "" {
		t.Fatal("直播流应报错")
	}
}
