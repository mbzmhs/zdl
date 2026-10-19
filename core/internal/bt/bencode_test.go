package bt

import (
	"bytes"
	"crypto/sha1"
	"reflect"
	"strings"
	"testing"
)

func TestEncodeKnown(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{int64(42), "i42e"},
		{int(-7), "i-7e"},
		{int64(0), "i0e"},
		{"spam", "4:spam"},
		{"", "0:"},
		{"你好", "6:你好"},
		{[]byte{0, 1, 2}, "3:\x00\x01\x02"},
		{[]any{}, "le"},
		{[]any{int64(1), "two"}, "li1e3:twoe"},
		{map[string]any{}, "de"},
		// 键必须按字节序输出
		{map[string]any{"b": int64(2), "a": int64(1)}, "d1:ai1e1:bi2ee"},
		{map[string]any{"m": []any{int64(1), int64(2)}}, "d1:mli1ei2eee"},
		{Raw([]byte("i9e")), "i9e"},
		{map[string]any{"info": Raw([]byte("d1:ai1ee"))}, "d4:infod1:ai1eee"},
	}
	for _, c := range cases {
		got, err := Encode(c.in)
		if err != nil {
			t.Fatalf("Encode(%v): %v", c.in, err)
		}
		if string(got) != c.want {
			t.Errorf("Encode(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEncodeUnsupported(t *testing.T) {
	if _, err := Encode(true); err == nil {
		t.Error("Encode(bool) 应返回错误")
	}
	if _, err := Encode(1.5); err == nil {
		t.Error("Encode(float) 应返回错误")
	}
}

func TestDecodeKnown(t *testing.T) {
	v, err := Decode([]byte("i-300e"))
	if err != nil || v != int64(-300) {
		t.Fatalf("i-300e => %v, %v", v, err)
	}
	v, err = Decode([]byte("d3:bar4:spam3:fooli1ei2eee"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"bar": "spam", "foo": []any{int64(1), int64(2)}}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("dict => %#v", v)
	}
	// 空容器必须物化为非 nil
	v, _ = Decode([]byte("de"))
	if _, ok := v.(map[string]any); !ok || v == nil {
		t.Fatalf("de => %#v", v)
	}
	// 二进制字符串承载原始字节
	v, _ = Decode([]byte("3:\x00\xff\x01"))
	if s, ok := v.(string); !ok || s != "\x00\xff\x01" {
		t.Fatalf("binary => %#v", v)
	}
}

func TestRoundTrip(t *testing.T) {
	vals := []any{
		int64(1 << 62), "hello world", string([]byte{0, 1, 2, 253}),
		[]any{int64(1), "x", []any{}, map[string]any{}},
		map[string]any{
			"announce":      "http://t/announce",
			"creation date": int64(1700000000),
			"info": map[string]any{
				"name":         "file.bin",
				"piece length": int64(16384),
				"pieces":       string(bytes.Repeat([]byte{0xAB}, 40)),
				"length":       int64(12345),
			},
			"nested": []any{map[string]any{"deep": int64(-5)}},
		},
	}
	for i, v := range vals {
		enc, err := Encode(v)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		dec, err := Decode(enc)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if !reflect.DeepEqual(v, dec) {
			t.Fatalf("case %d 往返不一致:\n got %#v\nwant %#v", i, dec, v)
		}
	}
}

func TestDecodeErrors(t *testing.T) {
	bad := []string{
		"",                         // 空
		"i12",                      // 整数未闭合
		"ixae",                     // 非法整数
		"i9999999999999999999999e", // 溢出
		"5:hel",                    // 字符串截断
		"a:hello",                  // 非法长度
		"99999999999:a",            // 超长字符串
		"x",                        // 非法类型
		"-",                        // 非法类型
		"d1:a",                     // dict 截断
		"di1e4:spame",              // dict 键非字符串
		"l1:a",                     // list 截断
		"i1ei2e",                   // 尾部多余数据
		"l" + strings.Repeat("l", 100) + strings.Repeat("e", 100) + "e", // 深度超限
	}
	for _, s := range bad {
		if _, err := Decode([]byte(s)); err == nil {
			t.Errorf("Decode(%q) 应返回错误", s)
		}
	}
}

func TestDecodePrefix(t *testing.T) {
	// dict + 尾随原始数据(BEP-9 元数据消息形态)
	data := []byte("d8:msg_typei1e5:piecei0ee" + strings.Repeat("M", 20))
	v, n, err := DecodePrefix(data)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := v.(map[string]any)
	if !ok || d["piece"] != int64(0) {
		t.Fatalf("prefix => %#v", v)
	}
	if n != len(data)-20 {
		t.Fatalf("消耗字节数 = %d, want %d", n, len(data)-20)
	}
}

func TestInfoDictSpan(t *testing.T) {
	infoRaw := []byte("d6:lengthi5e4:name5:hello6:pieces0:e")
	data := []byte("d8:announce25:http://t.example/announce4:info" + string(infoRaw) +
		"8:created by4:zd-1e")
	start, end, err := InfoDictSpan(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data[start:end], infoRaw) {
		t.Fatalf("span = %q, want %q", data[start:end], infoRaw)
	}
	// infohash 必须基于原始字节:重构同样的 info dict,哈希应当一致
	sum1 := sha1.Sum(data[start:end])
	enc, _ := Encode(map[string]any{"length": int64(5), "name": "hello", "pieces": ""})
	sum2 := sha1.Sum(enc)
	if sum1 != sum2 {
		t.Fatalf("infohash 不一致: %x vs %x", sum1, sum2)
	}
	// 值为字符串的 "info"(非法形态)也能正确跳过后继续检索? 这里 info 是最后一个键的对照组:
	// 交换键序后仍能找到
	data2 := []byte("d4:infod6:lengthi5e4:name5:hello6:pieces0:ee")
	s2, e2, err := InfoDictSpan(data2)
	if err != nil || !bytes.Equal(data2[s2:e2], infoRaw) {
		t.Fatalf("info 在前: %q %v", data2[s2:e2], err)
	}
}

func TestInfoDictSpanErrors(t *testing.T) {
	for _, s := range []string{
		"", "i1e", "de", // 无 info
		"d4:info",        // 截断
		"d4:infod4:name", // info 值截断
	} {
		if _, _, err := InfoDictSpan([]byte(s)); err == nil {
			t.Errorf("InfoDictSpan(%q) 应返回错误", s)
		}
	}
}

func TestSkipValueStringSpan(t *testing.T) {
	// 长字符串值正确跳过(内含 bencode 形态的假数据)
	data := []byte("d1:a12:xxd1:bi1eeyy1:b2:cce")
	end, err := skipValue(data, 4, 1)
	if err != nil {
		t.Fatal(err)
	}
	if data[end] != '1' { // 接下来应是键 "1:b"
		t.Fatalf("skip 后位置错误: %q", data[end:])
	}
}
