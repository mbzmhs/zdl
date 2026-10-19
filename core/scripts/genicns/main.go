// genicns 生成 macOS .app 图标:加载仓库根目录的品牌图标 icon.png(圆角方块
// 深蓝渐变底 + 亮色 Z/下载箭头),缩放出 icns 全部尺寸写入 AppIcon.iconset/,
// 由 build.sh 调 iconutil 打成 .icns。面积平均缩放,1024px 与 16px 同源同形。
package main

import (
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
)

// srcPath 定位仓库根的 icon.png,与调用时的 cwd 无关
// (本文件 → scripts/genicns → scripts → core → 仓库根)。
func srcPath() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "icon.png"
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "icon.png"))
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

var icnsSizes = []struct {
	name string
	px   int
}{
	{"16x16", 16}, {"16x16@2x", 32},
	{"32x32", 32}, {"32x32@2x", 64},
	{"128x128", 128}, {"128x128@2x", 256},
	{"256x256", 256}, {"256x256@2x", 512},
	{"512x512", 512}, {"512x512@2x", 1024},
}

func main() {
	outDir := "AppIcon.iconset"
	if len(os.Args) > 1 {
		outDir = os.Args[1]
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
	sq := padSquare(src)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, s := range icnsSizes {
		o, err := os.Create(filepath.Join(outDir, "icon_"+s.name+".png"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := png.Encode(o, resizeBox(sq, s.px, s.px)); err != nil {
			o.Close()
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		o.Close()
	}
	fmt.Println("iconset written:", outDir)
}
