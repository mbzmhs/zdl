// genicons 从仓库根目录的品牌图标 icon.png 生成扩展图标(16/32/48/128)。
// 可选第二参数同时输出多尺寸 .ico(Windows 应用图标资源,供 genico 打进 .syso)。
// 面积平均缩放,与 genicns 共用同一源图,全平台图标同源同形。
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
)

// srcPath 定位仓库根的 icon.png,与调用时的 cwd 无关
// (本文件 → scripts/genicons → scripts → core → 仓库根)。
func srcPath() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "icon.png"
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "icon.png"))
}

// defaultOutDir 默认输出到仓库根的 extension/icons(同 srcPath 定位法,不受 cwd 影响)。
func defaultOutDir() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "extension/icons"
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "extension", "icons"))
}

// padSquare 把非正方图补到 max(w,h) 的透明画布,水平/垂直居中。
func padSquare(src image.Image) *image.NRGBA {
	b := src.Bounds()
	n := b.Dx()
	if b.Dy() > n {
		n = b.Dy()
	}
	out := image.NewNRGBA(image.Rect(0, 0, n, n))
	ox, oy := (n-b.Dx())/2, (n-b.Dy())/2
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			r, g, bl, a := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
			i := out.PixOffset(ox+x, oy+y)
			out.Pix[i], out.Pix[i+1], out.Pix[i+2], out.Pix[i+3] = uint8(r>>8), uint8(g>>8), uint8(bl>>8), uint8(a>>8)
		}
	}
	return out
}

// resizeBox 面积平均缩放:目标像素取覆盖源像素的加权平均(权重=重叠面积)。
func resizeBox(src *image.NRGBA, w, h int) *image.NRGBA {
	b := src.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	xr, yr := float64(b.Dx())/float64(w), float64(b.Dy())/float64(h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			sx0, sy0 := float64(x)*xr, float64(y)*yr
			sx1, sy1 := sx0+xr, sy0+yr
			var sr, sg, sb, sa, tw float64
			for sy := int(sy0); sy < int(sy1+1) && sy < b.Dy(); sy++ {
				oy0, oy1 := float64(sy), float64(sy+1)
				if oy0 < sy0 {
					oy0 = sy0
				}
				if oy1 > sy1 {
					oy1 = sy1
				}
				for sx := int(sx0); sx < int(sx1+1) && sx < b.Dx(); sx++ {
					ox0, ox1 := float64(sx), float64(sx+1)
					if ox0 < sx0 {
						ox0 = sx0
					}
					if ox1 > sx1 {
						ox1 = sx1
					}
					wgt := (oy1 - oy0) * (ox1 - ox0)
					if wgt <= 0 {
						continue
					}
					i := src.PixOffset(sx, sy)
					sr += float64(src.Pix[i]) * wgt
					sg += float64(src.Pix[i+1]) * wgt
					sb += float64(src.Pix[i+2]) * wgt
					sa += float64(src.Pix[i+3]) * wgt
					tw += wgt
				}
			}
			i := out.PixOffset(x, y)
			out.Pix[i] = uint8(sr / tw)
			out.Pix[i+1] = uint8(sg / tw)
			out.Pix[i+2] = uint8(sb / tw)
			out.Pix[i+3] = uint8(sa / tw)
		}
	}
	return out
}

func savePNG(img image.Image, path string) {
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
}

// genICO 打包多尺寸 .ico:目录条目 + 各尺寸 PNG 数据
// (Vista+ 支持 PNG 压缩图标条目,WebView2 场景只需覆盖现代系统)。
func genICO(master *image.NRGBA, sizes []int, path string) {
	type entry struct {
		size   int
		data   []byte
		offset uint32
	}
	var entries []entry
	for _, s := range sizes {
		var buf bytes.Buffer
		if err := png.Encode(&buf, resizeBox(master, s, s)); err != nil {
			panic(err)
		}
		entries = append(entries, entry{size: s, data: buf.Bytes()})
	}
	headerSize := uint32(6 + 16*len(entries))
	off := headerSize
	for i := range entries {
		entries[i].offset = off
		off += uint32(len(entries[i].data))
	}
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	le := binary.LittleEndian
	w := bufio.NewWriter(f)
	// ICONDIR:保留 0、类型 1(图标)、条目数
	binary.Write(w, le, uint16(0))
	binary.Write(w, le, uint16(1))
	binary.Write(w, le, uint16(len(entries)))
	for _, e := range entries {
		dim := uint8(e.size % 256)      // 256 写 0
		binary.Write(w, le, dim)        // 宽
		binary.Write(w, le, dim)        // 高
		w.WriteByte(0)                  // 调色板色数
		w.WriteByte(0)                  // 保留
		binary.Write(w, le, uint16(1))  // 色平面
		binary.Write(w, le, uint16(32)) // 位深
		binary.Write(w, le, uint32(len(e.data)))
		binary.Write(w, le, e.offset)
	}
	for _, e := range entries {
		w.Write(e.data)
	}
	if err := w.Flush(); err != nil {
		panic(err)
	}
}

func main() {
	out := defaultOutDir()
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	f, err := os.Open(srcPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开品牌图标失败(仓库根 icon.png):", err)
		os.Exit(1)
	}
	src, err := png.Decode(f)
	f.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "解析品牌图标失败:", err)
		os.Exit(1)
	}
	master := padSquare(src)

	_ = os.MkdirAll(out, 0o755)
	for _, s := range []int{16, 32, 48, 128} {
		savePNG(resizeBox(master, s, s), filepath.Join(out, fmt.Sprintf("icon%d.png", s)))
	}
	fmt.Println("icons generated to", out)

	// 第二个参数可选:同时输出多尺寸 .ico(Windows 应用图标资源)
	if len(os.Args) > 2 {
		icoPath := os.Args[2]
		genICO(master, []int{16, 24, 32, 48, 64, 128, 256}, icoPath)
		fmt.Println("ico generated:", icoPath)
	}
}
