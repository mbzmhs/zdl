package rss

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Parsed 是一次抓取解析出的订阅内容(归一化后)。
type Parsed struct {
	Title string // 频道标题
	Link  string // 站点主页
	Items []FetchedItem
}

// FetchedItem 是解析出的单条条目(尚未分配本地 ID)。
type FetchedItem struct {
	GUID    string // 稳定标识(guid / atom id),可空
	Title   string
	Link    string // 条目页面链接
	Date    string // RFC3339 归一化;不可解析时保留原文
	Summary string // 纯文本摘要(截断)
	Links   []ItemLink
}

// ---- XML 结构(RSS 2.0 / RSS 1.0 RDF / Atom 三类根共用一套解码目标)----

type xmlLink struct {
	Rel    string `xml:"rel,attr"`
	Href   string `xml:"href,attr"`
	Type   string `xml:"type,attr"`
	Length int64  `xml:"length,attr"`
	Text   string `xml:",chardata"`
}

type xmlEnclosure struct {
	URL    string `xml:"url,attr"`
	Type   string `xml:"type,attr"`
	Length int64  `xml:"length,attr"`
}

type xmlItem struct {
	Title    string         `xml:"title"`
	Links    []xmlLink      `xml:"link"`
	GUID     string         `xml:"guid"`
	PubDate  string         `xml:"pubDate"`
	Date     string         `xml:"date"`
	Desc     string         `xml:"description"`
	Content  string         `xml:"encoded"`
	Enc      []xmlEnclosure `xml:"enclosure"`
	Media    []xmlEnclosure `xml:"content"` // media:content(带 url 属性的才有效)
	Comments string         `xml:"comments"`
}

type xmlEntry struct {
	Title     string         `xml:"title"`
	Links     []xmlLink      `xml:"link"`
	ID        string         `xml:"id"`
	Updated   string         `xml:"updated"`
	Published string         `xml:"published"`
	Summary   string         `xml:"summary"`
	Content   string         `xml:"content"`
}

type xmlDoc struct {
	XMLName xml.Name `xml:""`
	// RSS 2.0
	Channel struct {
		Title string    `xml:"title"`
		Link  []xmlLink `xml:"link"`
		Items []xmlItem `xml:"item"`
	} `xml:"channel"`
	// Atom
	Title   string     `xml:"title"`
	Links   []xmlLink  `xml:"link"`
	Entries []xmlEntry `xml:"entry"`
	// RSS 1.0(RDF):item 与 channel 平级
	Items []xmlItem `xml:"item"`
}

// Parse 解析 RSS 2.0 / RSS 1.0(RDF)/ Atom 内容。
// 非严格模式:真实世界的订阅源常带未定义实体(&nbsp; 等)与不规范标签,
// 严格模式会整篇失败;非严格模式下这些内容按原文透传。
func Parse(data []byte) (*Parsed, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = false
	d.CharsetReader = charsetReader
	var doc xmlDoc
	if err := d.Decode(&doc); err != nil {
		return nil, fmt.Errorf("XML 解析失败: %w", err)
	}
	switch {
	case doc.Channel.Title != "" || len(doc.Channel.Items) > 0:
		p := &Parsed{
			Title: cleanText(doc.Channel.Title),
			Link:  pickPageLink(doc.Channel.Link),
		}
		items := doc.Channel.Items
		if len(items) == 0 {
			items = doc.Items // RSS 1.0
		}
		for _, x := range items {
			p.Items = append(p.Items, convertRSSItem(x))
		}
		if p.Title == "" && len(p.Items) == 0 {
			return nil, errors.New("订阅内容为空")
		}
		return p, nil
	case len(doc.Entries) > 0:
		p := &Parsed{Title: cleanText(doc.Title), Link: pickPageLink(doc.Links)}
		for _, x := range doc.Entries {
			p.Items = append(p.Items, convertAtomEntry(x))
		}
		return p, nil
	}
	return nil, errors.New("不是有效的 RSS/Atom 订阅内容")
}

func convertRSSItem(x xmlItem) FetchedItem {
	it := FetchedItem{
		GUID:    strings.TrimSpace(x.GUID),
		Title:   cleanText(x.Title),
		Link:    pickPageLink(x.Links),
		Date:    normDate(firstNonEmpty(x.PubDate, x.Date)),
		Summary: summarize(firstNonEmpty(x.Desc, x.Content), 240),
	}
	it.Links = pickEnclosures(x.Enc, x.Media, x.Links)
	if it.Title == "" {
		it.Title = it.Link
	}
	return it
}

func convertAtomEntry(x xmlEntry) FetchedItem {
	it := FetchedItem{
		GUID:    strings.TrimSpace(x.ID),
		Title:   cleanText(x.Title),
		Link:    pickPageLink(x.Links),
		Date:    normDate(firstNonEmpty(x.Published, x.Updated)),
		Summary: summarize(firstNonEmpty(x.Summary, x.Content), 240),
	}
	it.Links = pickEnclosures(nil, nil, x.Links)
	if it.Title == "" {
		it.Title = it.Link
	}
	return it
}

// pickPageLink 取条目/频道页面链接:优先无 rel 或 rel=alternate 的项。
func pickPageLink(ls []xmlLink) string {
	for _, l := range ls {
		if l.Rel != "" && l.Rel != "alternate" {
			continue
		}
		if u := normURL(firstNonEmpty(l.Text, l.Href)); u != "" {
			return u
		}
	}
	return ""
}

// pickEnclosures 汇总可下载资源链接:RSS enclosure / media:content 与
// Atom rel=enclosure,按 URL 去重。
func pickEnclosures(encs, media []xmlEnclosure, ls []xmlLink) []ItemLink {
	var out []ItemLink
	seen := map[string]bool{}
	add := func(u, typ string, length int64, title string) {
		u = normURL(u)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, ItemLink{URL: u, Type: strings.TrimSpace(typ), Length: length, Title: cleanText(title)})
	}
	for _, e := range encs {
		add(e.URL, e.Type, e.Length, "")
	}
	for _, e := range media {
		add(e.URL, e.Type, e.Length, "")
	}
	for _, l := range ls {
		if l.Rel == "enclosure" {
			add(firstNonEmpty(l.Href, l.Text), l.Type, l.Length, "")
		}
	}
	return out
}

// normURL 仅保留可下载/可打开的地址(http/https/magnet)。
func normURL(u string) string {
	u = strings.TrimSpace(u)
	lu := strings.ToLower(u)
	switch {
	case strings.HasPrefix(lu, "http://"), strings.HasPrefix(lu, "https://"), strings.HasPrefix(lu, "magnet:"):
		return u
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// cleanText 折叠空白并解码 HTML 实体(非严格模式下未定义实体按原文保留)。
func cleanText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return html.UnescapeString(s)
}

var tagRe = regexp.MustCompile(`(?s)<[^>]*>`)

// summarize 去标签取纯文本摘要并截断。
func summarize(s string, max int) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	s = html.UnescapeString(tagRe.ReplaceAllString(s, " "))
	s = strings.Join(strings.Fields(s), " ")
	if max > 0 && utf8.RuneCountInString(s) > max {
		r := []rune(s)
		s = string(r[:max]) + "…"
	}
	return s
}

var dateLayouts = []string{
	time.RFC3339Nano, time.RFC3339,
	time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822,
	time.ANSIC, time.UnixDate, time.RubyDate,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"2 Jan 2006 15:04:05 -0700",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"2006/01/02 15:04:05",
	"2006/01/02",
}

// normDate 把常见发布时间格式归一化为 RFC3339;无法识别时保留原文。
func normDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.Format(time.RFC3339)
		}
	}
	return s
}

// charsetReader 处理 XML 声明的字符集:UTF-8 直接用;Latin-1 系转 UTF-8;
// 其余(如 GBK)原样透传——标签结构仍可解析,仅非 ASCII 正文可能乱码。
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return input, nil
	case "iso-8859-1", "latin1", "latin-1", "windows-1252":
		return &latin1Reader{r: input}, nil
	default:
		return input, nil
	}
}

// latin1Reader 把单字节 Latin-1 流转换为 UTF-8。
type latin1Reader struct{ r io.Reader }

func (l *latin1Reader) Read(p []byte) (int, error) {
	if len(p) < 2 {
		return 0, io.ErrShortBuffer
	}
	buf := make([]byte, len(p)/2)
	n, err := l.r.Read(buf)
	out := 0
	for _, b := range buf[:n] {
		if b < 0x80 {
			p[out] = b
			out++
		} else {
			p[out] = 0xC0 | b>>6
			p[out+1] = 0x80 | b&0x3F
			out += 2
		}
	}
	if out > 0 {
		return out, nil
	}
	return 0, err
}
