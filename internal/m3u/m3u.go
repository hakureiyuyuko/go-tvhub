// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

// Package m3u 负责解析与生成直播源播放列表（M3U / M3U8）。
package m3u

import (
	"fmt"
	"regexp"
	"strings"
)

// Entry 是播放列表中的一条直播源。
type Entry struct {
	Name    string // 频道名
	URL     string // 播放地址（已剥离 "|" 之后的附加请求头）
	Group   string // 分组（group-title）
	Logo    string // tvg-logo
	TvgID   string // tvg-id
	TvgName string // tvg-name
	Headers string // 附加请求头，形如 "User-Agent: xxx\r\n"
}

var attrRe = regexp.MustCompile(`([A-Za-z0-9_.\-]+)\s*=\s*"([^"]*)"`)

// Parse 解析 m3u/m3u8 文本。解析失败的条目会被跳过（例如只有 #EXTINF 却没有地址）。
func Parse(content string) []Entry {
	content = strings.TrimPrefix(content, "\ufeff")
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	var (
		out   []Entry
		cur   *Entry
		group string
	)
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			upper := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(upper, "#EXTINF"):
				e := parseExtinf(line)
				cur = &e
				group = ""
			case strings.HasPrefix(upper, "#EXTGRP:"):
				group = strings.TrimSpace(line[len("#EXTGRP:"):])
				if cur != nil {
					cur.Group = group
				}
			default:
				// #EXTM3U / #KODIPROP / #EXTVLCOPT 等指令忽略
			}
			continue
		}
		if cur == nil {
			// 没有 #EXTINF 的裸地址：用地址本身当名字
			url, headers := splitHeaders(line)
			if isStreamURL(url) {
				out = append(out, Entry{Name: url, URL: url, Headers: headers})
			}
			continue
		}
		e := *cur
		url, headers := splitHeaders(line)
		if url == "" {
			cur = nil
			continue
		}
		e.URL, e.Headers = url, headers
		if e.Group == "" {
			e.Group = group
		}
		if e.Name == "" {
			e.Name = e.TvgName
		}
		if e.Name == "" {
			e.Name = url
		}
		out = append(out, e)
		cur = nil
		group = ""
	}
	return Dedupe(out)
}

// parseExtinf 解析形如 `#EXTINF:-1 tvg-id="x" group-title="y",频道名` 的行。
func parseExtinf(line string) Entry {
	var e Entry
	rest := line[len("#EXTINF"):] // "#EXTINF" 长度固定，与大小写无关
	attrs, name := rest, ""
	if i := indexUnquoted(rest, ','); i >= 0 {
		attrs, name = rest[:i], rest[i+1:]
	}
	for _, m := range attrRe.FindAllStringSubmatch(attrs, -1) {
		key, val := strings.ToLower(m[1]), strings.TrimSpace(m[2])
		switch key {
		case "tvg-id":
			e.TvgID = val
		case "tvg-name":
			e.TvgName = val
		case "tvg-logo":
			e.Logo = val
		case "group-title", "group":
			e.Group = val
		}
	}
	e.Name = strings.TrimSpace(name)
	return e
}

// indexUnquoted 返回 s 中第一个不在双引号内的 c 的下标，找不到返回 -1。
func indexUnquoted(s string, c byte) int {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case c:
			if !inQuote {
				return i
			}
		}
	}
	return -1
}

// splitHeaders 处理 `http://x/y|User-Agent=foo&Referer=bar` 这种写法。
func splitHeaders(line string) (string, string) {
	i := strings.Index(line, "|")
	if i < 0 {
		return strings.TrimSpace(line), ""
	}
	url := strings.TrimSpace(line[:i])
	var sb strings.Builder
	for _, part := range strings.FieldsFunc(line[i+1:], func(r rune) bool { return r == '|' || r == '&' }) {
		k, v, ok := strings.Cut(part, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			continue
		}
		sb.WriteString(k)
		sb.WriteString(": ")
		sb.WriteString(v)
		sb.WriteString("\r\n")
	}
	return url, sb.String()
}

func isStreamURL(s string) bool {
	l := strings.ToLower(s)
	for _, p := range []string{"rtsp://", "rtsps://", "rtmp://", "http://", "https://", "udp://", "rtp://", "file://"} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// Dedupe 按地址去重，保留最先出现的一条。
func Dedupe(in []Entry) []Entry {
	seen := make(map[string]bool, len(in))
	out := make([]Entry, 0, len(in))
	for _, e := range in {
		if e.URL == "" || seen[e.URL] {
			continue
		}
		seen[e.URL] = true
		out = append(out, e)
	}
	return out
}

// ApplyGroups 为没有分组的条目推断分组，并统一分组名格式。
func ApplyGroups(in []Entry) []Entry {
	out := make([]Entry, len(in))
	for i, e := range in {
		e.Group = strings.TrimSpace(e.Group)
		if e.Group == "" {
			e.Group = GuessGroup(e.Name)
		}
		out[i] = e
	}
	return out
}

// Render 把条目还原成 m3u 文本。
func Render(in []Entry) string {
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	for _, e := range in {
		sb.WriteString("#EXTINF:-1")
		if e.TvgID != "" {
			fmt.Fprintf(&sb, ` tvg-id="%s"`, e.TvgID)
		}
		if e.TvgName != "" {
			fmt.Fprintf(&sb, ` tvg-name="%s"`, e.TvgName)
		}
		if e.Logo != "" {
			fmt.Fprintf(&sb, ` tvg-logo="%s"`, e.Logo)
		}
		if e.Group != "" {
			fmt.Fprintf(&sb, ` group-title="%s"`, e.Group)
		}
		sb.WriteString(",")
		sb.WriteString(strings.TrimSpace(e.Name))
		sb.WriteString("\n")
		sb.WriteString(e.URL)
		for _, line := range strings.Split(strings.TrimSpace(e.Headers), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok {
				continue
			}
			sb.WriteString("|" + strings.TrimSpace(k) + "=" + strings.TrimSpace(v))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// Kind 返回地址的协议类型，用于选择转发方式。
func Kind(rawurl string) string {
	l := strings.ToLower(strings.TrimSpace(rawurl))
	switch {
	case strings.HasPrefix(l, "rtsp://"), strings.HasPrefix(l, "rtsps://"):
		return "rtsp"
	case strings.HasPrefix(l, "rtmp://"), strings.HasPrefix(l, "rtmps://"):
		return "rtmp"
	case strings.HasPrefix(l, "udp://"), strings.HasPrefix(l, "rtp://"):
		return "udp"
	case strings.HasPrefix(l, "http://"), strings.HasPrefix(l, "https://"):
		return "http"
	}
	return "other"
}
