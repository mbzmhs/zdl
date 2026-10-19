package bt

import (
	"regexp"
)

// bitCometPaddingRe 匹配 BitComet 0.85+ 制种的填充文件命名(____padding_file_N_…
// 后缀任意),用于把后续文件推到 piece 边界;BEP-47 规范形态则是 .pad/<字节数>。
// 两者内容都保证全零,只是对齐占位——落盘无意义(读按零、写丢弃、绝不建文件),
// 见 Storage 的 pad 虚拟化。
var bitCometPaddingRe = regexp.MustCompile(`^_{2,}padding_file_\d+_`)

// IsPaddingPath 报告种子内文件相对路径(分段形式)是否为填充文件。
func IsPaddingPath(segments []string) bool {
	if len(segments) == 0 {
		return false
	}
	if bitCometPaddingRe.MatchString(segments[len(segments)-1]) {
		return true
	}
	for _, seg := range segments[:len(segments)-1] {
		if seg == ".pad" { // BEP-47:填充文件一律位于 .pad 目录下
			return true
		}
	}
	return false
}

// paddingMask 返回各文件是否为填充文件(下标与 Files 对齐;单文件种子全 false)。
func (m *MetaInfo) paddingMask() []bool {
	mask := make([]bool, len(m.Files))
	for i, f := range m.Files {
		mask[i] = IsPaddingPath(f.Path)
	}
	return mask
}
