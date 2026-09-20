// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package m3u

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBasic(t *testing.T) {
	src := "#EXTM3U\n" +
		"#EXTINF:-1 ,CCTV1 综合\n" +
		"rtsp://10.0.0.1:554/a?token=x\n" +
		"#EXTINF:-1 tvg-id=\"cctv2\" tvg-logo=\"http://l/2.png\" group-title=\"央视\",CCTV2 财经\n" +
		"http://10.0.0.2/live/2.m3u8\n" +
		"#EXTINF:-1,带请求头\n" +
		"http://10.0.0.3/x.m3u8|User-Agent=Mozilla/5.0&Referer=http://a/\n" +
		"#EXTINF:-1,只有名字没有地址\n"
	got := Parse(src)
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d: %+v", len(got), got)
	}
	if got[0].Name != "CCTV1 综合" || got[0].Group != "" {
		t.Errorf("entry0 = %+v", got[0])
	}
	if got[1].TvgID != "cctv2" || got[1].Logo != "http://l/2.png" || got[1].Group != "央视" {
		t.Errorf("entry1 = %+v", got[1])
	}
	if got[2].URL != "http://10.0.0.3/x.m3u8" || !strings.Contains(got[2].Headers, "User-Agent: Mozilla/5.0") ||
		!strings.Contains(got[2].Headers, "Referer: http://a/") {
		t.Errorf("entry2 = %+v", got[2])
	}
}

func TestParseExtGrpAndDedupe(t *testing.T) {
	src := "#EXTM3U\n#EXTINF:-1,A\n#EXTGRP:我的组\nrtsp://a/1\n#EXTINF:-1,A\nrtsp://a/1\n"
	got := Parse(src)
	if len(got) != 1 {
		t.Fatalf("dedupe failed: %+v", got)
	}
	if got[0].Group != "我的组" {
		t.Errorf("EXTGRP not applied: %+v", got[0])
	}
}

func TestRenderRoundTrip(t *testing.T) {
	in := []Entry{{Name: "频道,1", URL: "http://a/x", Group: "G", Logo: "l.png", Headers: "User-Agent: ua\r\n"}}
	out := Parse(Render(in))
	if len(out) != 1 || out[0].Name != "频道,1" || out[0].URL != "http://a/x" || out[0].Group != "G" {
		t.Fatalf("round trip failed: %+v", out)
	}
	if !strings.Contains(out[0].Headers, "User-Agent: ua") {
		t.Errorf("headers lost: %+v", out[0])
	}
}

func TestGuessGroup(t *testing.T) {
	cases := map[string]string{
		"CCTV5 体育":         "央视",
		"湖南卫视 4K HEVC HDR": "卫视",
		"重庆新闻":             "重庆",
		"上海纪实人文":           "上海",
		"CHC 动作电影":         "影视剧场",
		"IPTV 谍战剧场 SD":     "IPTV专区",
		"未知频道":             "其他",
	}
	for name, want := range cases {
		if got := GuessGroup(name); got != want {
			t.Errorf("GuessGroup(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestKind(t *testing.T) {
	cases := map[string]string{
		"rtsp://a/1":       "rtsp",
		"http://a/1.m3u8":  "http",
		"udp://@239.0.0.1": "udp",
		"rtmp://a/live":    "rtmp",
	}
	for in, want := range cases {
		if got := Kind(in); got != want {
			t.Errorf("Kind(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestParseSampleFixture 用 testdata 里的样例播放列表做冒烟解析。
func TestParseSampleFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "sample.m3u"))
	if err != nil {
		t.Fatalf("读取样例失败: %v", err)
	}
	got := ApplyGroups(Parse(string(b)))
	if len(got) < 8 {
		t.Fatalf("期望至少 8 个频道，实际 %d", len(got))
	}
	groups := map[string]int{}
	kinds := map[string]int{}
	for _, e := range got {
		groups[e.Group]++
		kinds[Kind(e.URL)]++
		if e.URL == "" || e.Name == "" {
			t.Fatalf("字段为空: %+v", e)
		}
	}
	t.Logf("解析出 %d 个频道，分组=%v，协议=%v", len(got), groups, kinds)
	for _, want := range []string{"央视", "卫视", "重庆", "上海", "影视剧场", "IPTV专区", "少儿教育"} {
		if groups[want] == 0 {
			t.Errorf("缺少分组 %q", want)
		}
	}
	if kinds["rtsp"] == 0 || kinds["http"] == 0 {
		t.Errorf("协议识别异常: %v", kinds)
	}
}
