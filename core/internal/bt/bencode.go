// Package bt 实现零依赖 BitTorrent 客户端引擎。
// 覆盖:bencode/metainfo(BEP-3)、tracker HTTP(BEP-3/23)与 UDP(BEP-15)、
// peer wire、扩展协议与元数据交换(BEP-10/9)、PEX(BEP-11)、DHT(BEP-5)。
package bt

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
)

const (
	// maxDepth 限制 bencode 嵌套深度,防御恶意构造的栈消耗。
	maxDepth = 64
	// maxStr 限制单个字符串(字节串)长度。种子 pieces 字段最坏情况
	//(1TB 资源、256KiB piece)约 80MB 超限,但 v1 引擎以 16MB 为安全上限,
	// 超大种子在 metainfo 层给出明确报错。
	maxStr = 1 << 24
)

// Decode 解析完整 bencode 数据。返回值为 int64 / string / []any / map[string]any。
// 键或值里的二进制数据(如 info_hash、compact peers)以 string 承载原始字节。
func Decode(data []byte) (any, error) {
	v, n, err := decode(data, 0, 0)
	if err != nil {
		return nil, err
	}
	if n != len(data) {
		return nil, fmt.Errorf("bencode: 尾部多余 %d 字节", len(data)-n)
	}
	return v, nil
}

// DecodePrefix 解析首部的一个 bencode 值,返回值与消耗的字节数。
// BEP-9 元数据消息 = bencode dict + 原始二进制数据,需要前缀式解码。
func DecodePrefix(data []byte) (any, int, error) {
	return decode(data, 0, 0)
}

func decode(data []byte, pos, depth int) (any, int, error) {
	if depth > maxDepth {
		return nil, 0, errors.New("bencode: 嵌套过深")
	}
	if pos >= len(data) {
		return nil, 0, errors.New("bencode: 数据截断")
	}
	switch c := data[pos]; {
	case c == 'i':
		end := pos + 1
		for end < len(data) && data[end] != 'e' {
			end++
		}
		if end >= len(data) {
			return nil, 0, errors.New("bencode: 整数未闭合")
		}
		n, err := strconv.ParseInt(string(data[pos+1:end]), 10, 64)
		if err != nil {
			return nil, 0, fmt.Errorf("bencode: 非法整数 %q", data[pos:end+1])
		}
		return n, end + 1, nil
	case c >= '0' && c <= '9':
		colon := pos + 1
		for colon < len(data) && data[colon] != ':' {
			colon++
		}
		if colon >= len(data) {
			return nil, 0, errors.New("bencode: 字符串长度未闭合")
		}
		n64, err := strconv.ParseInt(string(data[pos:colon]), 10, 64)
		if err != nil || n64 < 0 {
			return nil, 0, fmt.Errorf("bencode: 非法字符串长度 %q", data[pos:colon+1])
		}
		if n64 > maxStr {
			return nil, 0, fmt.Errorf("bencode: 字符串超长 %d", n64)
		}
		start, ln := colon+1, int(n64)
		if start+ln > len(data) {
			return nil, 0, errors.New("bencode: 字符串数据截断")
		}
		return string(data[start : start+ln]), start + ln, nil
	case c == 'l':
		pos++
		list := []any{}
		for {
			if pos < len(data) && data[pos] == 'e' {
				return list, pos + 1, nil
			}
			v, n, err := decode(data, pos, depth+1)
			if err != nil {
				return nil, 0, err
			}
			list = append(list, v)
			pos = n
		}
	case c == 'd':
		pos++
		dict := map[string]any{}
		for {
			if pos < len(data) && data[pos] == 'e' {
				return dict, pos + 1, nil
			}
			k, n, err := decode(data, pos, depth+1)
			if err != nil {
				return nil, 0, err
			}
			key, ok := k.(string)
			if !ok {
				return nil, 0, errors.New("bencode: dict 键必须是字符串")
			}
			v, n2, err := decode(data, n, depth+1)
			if err != nil {
				return nil, 0, err
			}
			dict[key] = v
			pos = n2
		}
	default:
		return nil, 0, fmt.Errorf("bencode: 非法类型字符 %q", c)
	}
}

// Raw 包装一段原样输出的 bencode 字节,用于手工拼接 info dict 等场景,
// 使 Encode 能把"已编码好的子树"嵌入更大的结构而不被二次转义。
type Raw []byte

// Encode 将 int / int64 / string / []byte / []any / map[string]any / Raw 编码为 bencode。
// map 键按字节序排序,输出规范形式;不支持的类型返回错误。
func Encode(v any) ([]byte, error) {
	return encode(nil, v)
}

// MustEncode 是 Encode 的便捷封装,仅用于内部构造可信数据。
func MustEncode(v any) []byte {
	b, err := Encode(v)
	if err != nil {
		panic(err)
	}
	return b
}

func encode(buf []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case int:
		buf = append(buf, 'i')
		buf = strconv.AppendInt(buf, int64(x), 10)
		return append(buf, 'e'), nil
	case int64:
		buf = append(buf, 'i')
		buf = strconv.AppendInt(buf, x, 10)
		return append(buf, 'e'), nil
	case uint32:
		buf = append(buf, 'i')
		buf = strconv.AppendUint(buf, uint64(x), 10)
		return append(buf, 'e'), nil
	case string:
		buf = strconv.AppendInt(buf, int64(len(x)), 10)
		buf = append(buf, ':')
		return append(buf, x...), nil
	case []byte:
		buf = strconv.AppendInt(buf, int64(len(x)), 10)
		buf = append(buf, ':')
		return append(buf, x...), nil
	case Raw:
		return append(buf, x...), nil
	case []any:
		buf = append(buf, 'l')
		for _, e := range x {
			var err error
			if buf, err = encode(buf, e); err != nil {
				return nil, err
			}
		}
		return append(buf, 'e'), nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf = append(buf, 'd')
		for _, k := range keys {
			buf = strconv.AppendInt(buf, int64(len(k)), 10)
			buf = append(buf, ':')
			buf = append(buf, k...)
			var err error
			if buf, err = encode(buf, x[k]); err != nil {
				return nil, err
			}
		}
		return append(buf, 'e'), nil
	default:
		return nil, fmt.Errorf("bencode: 不支持的类型 %T", v)
	}
}

// InfoDictSpan 返回顶层 dict 中 "info" 键对应值的原始字节范围 [start,end)。
// infohash 定义为 sha1(该切片),必须基于原始字节计算——重新序列化可能
// 因第三方客户端的非规范键序产生不同哈希。
func InfoDictSpan(data []byte) (start, end int, err error) {
	if len(data) == 0 || data[0] != 'd' {
		return 0, 0, errors.New("bencode: 种子顶层不是 dict")
	}
	pos := 1
	for {
		if pos >= len(data) {
			return 0, 0, errors.New("bencode: 顶层 dict 截断")
		}
		if data[pos] == 'e' {
			return 0, 0, errors.New("种子缺少 info 字段")
		}
		kv, n, err := decode(data, pos, 1)
		if err != nil {
			return 0, 0, err
		}
		key, ok := kv.(string)
		if !ok {
			return 0, 0, errors.New("bencode: dict 键必须是字符串")
		}
		if key == "info" {
			vend, err := skipValue(data, n, 1)
			if err != nil {
				return 0, 0, err
			}
			return n, vend, nil
		}
		if pos, err = skipValue(data, n, 1); err != nil {
			return 0, 0, err
		}
	}
}

// skipValue 校验并跳过一个 bencode 值,返回其结束位置(不含),不物化任何值。
func skipValue(data []byte, pos, depth int) (int, error) {
	if depth > maxDepth {
		return 0, errors.New("bencode: 嵌套过深")
	}
	if pos >= len(data) {
		return 0, errors.New("bencode: 数据截断")
	}
	switch data[pos] {
	case 'i':
		end := pos + 1
		for end < len(data) && data[end] != 'e' {
			end++
		}
		if end >= len(data) {
			return 0, errors.New("bencode: 整数未闭合")
		}
		if _, err := strconv.ParseInt(string(data[pos+1:end]), 10, 64); err != nil {
			return 0, fmt.Errorf("bencode: 非法整数 %q", data[pos:end+1])
		}
		return end + 1, nil
	case 'l', 'd':
		pos++
		for {
			if pos >= len(data) {
				return 0, errors.New("bencode: 容器截断")
			}
			if data[pos] == 'e' {
				return pos + 1, nil
			}
			var err error
			if pos, err = skipValue(data, pos, depth+1); err != nil {
				return 0, err
			}
		}
	default:
		if data[pos] < '0' || data[pos] > '9' {
			return 0, fmt.Errorf("bencode: 非法类型字符 %q", data[pos])
		}
		colon := pos + 1
		for colon < len(data) && data[colon] != ':' {
			colon++
		}
		if colon >= len(data) {
			return 0, errors.New("bencode: 字符串长度未闭合")
		}
		n64, err := strconv.ParseInt(string(data[pos:colon]), 10, 64)
		if err != nil || n64 < 0 || n64 > maxStr {
			return 0, fmt.Errorf("bencode: 非法字符串长度 %q", data[pos:colon+1])
		}
		end := colon + 1 + int(n64)
		if end > len(data) {
			return 0, errors.New("bencode: 字符串数据截断")
		}
		return end, nil
	}
}
