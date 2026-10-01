// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package stream

import (
	"testing"

	"tvhub/internal/store"
)

func TestDetectCodec(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{"  Stream #0:0[0x100]: Video: hevc (Main 10) (HEVC / 0x43564548), yuv420p10le(tv), 3840x2160 [SAR 1:1 DAR 16:9], 25 fps, 25 tbr, 90k tbn", "hevc"},
		{"  Stream #0:0, 21, 1/90000: Video: h264 (High), 1 reference frame ([27][0][0][0] / 0x001B), yuv420p(top first, left), 1920x1080, 25 fps", "h264"},
		{"  Stream #0:0: Video: mpeg2video (Main), yuv420p(tv, bt709), 720x576 [SAR 16:15 DAR 4:3]", "mpeg2video"},
		{"  Stream #0:1[0x101]: Audio: aac (LC), 48000 Hz, stereo, fltp", ""},
		{"[hls @ 0x62d3e6d3b300] Stream HEVC is not hvc1, you should use tag:v hvc1 to set it.", ""},
		{"  Stream #0:0: Video: h264 (High), yuv420p, 1920x1080 (copy)", "h264"},
	}
	for _, c := range cases {
		s := &Session{codecSeen: make(chan struct{})}
		s.detectCodec(c.line)
		if got := s.Codec(); got != c.want {
			t.Errorf("detectCodec(%q) = %q，期望 %q", c.line, got, c.want)
		}
	}
}

func TestDetectCodecKeepsFirst(t *testing.T) {
	s := &Session{codecSeen: make(chan struct{})}
	s.detectCodec("  Stream #0:0: Video: hevc (Main), yuv420p, 3840x2160")
	s.detectCodec("  Stream #0:0: Video: h264, yuv420p, 1920x1080") // 输出段，不能覆盖
	if got := s.Codec(); got != "hevc" {
		t.Errorf("应保留第一条（输入段）编码，得到 %q", got)
	}
}

func TestTranscodeDecision(t *testing.T) {
	rtsp := store.Channel{ID: 1, URL: "rtsp://example.com/1"}
	httpCh := store.Channel{ID: 2, URL: "http://example.com/1.m3u8"}
	cases := []struct {
		name  string
		mode  string
		codec string
		caps  ClientCaps
		ch    store.Channel
		want  bool
	}{
		{"auto + h264 → 不转", VideoModeAuto, "h264", ClientCaps{}, rtsp, false},
		{"auto + hevc → 转", VideoModeAuto, "hevc", ClientCaps{}, rtsp, true},
		{"auto + hevc + 客户端能放 → 不转", VideoModeAuto, "hevc", ClientCaps{HEVC: true}, rtsp, false},
		{"auto + mpeg2 → 转", VideoModeAuto, "mpeg2video", ClientCaps{}, rtsp, true},
		{"auto + 未知编码 → 先不转（等探明）", VideoModeAuto, "", ClientCaps{}, rtsp, false},
		{"copy → 一律不转", VideoModeCopy, "hevc", ClientCaps{}, rtsp, false},
		{"all → 一律转", VideoModeAll, "h264", ClientCaps{}, rtsp, true},
		{"http 源 → 从不由 ffmpeg 处理", VideoModeAll, "hevc", ClientCaps{}, httpCh, false},
	}
	for _, c := range cases {
		o := Options{VideoMode: c.mode, SourceCodec: c.codec, ClientHEVC: c.caps.HEVC}
		if got := o.Transcode(c.ch); got != c.want {
			t.Errorf("%s：Transcode() = %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestVAAPIFilter(t *testing.T) {
	o := Options{TranscodeHeight: 1080}
	if got := o.vaapiFilter(); got != "scale_vaapi=w=trunc(iw*1080/ih/2)*2:h=1080:format=nv12" {
		t.Errorf("1080p 滤镜不对: %s", got)
	}
	o.TranscodeHeight = 0
	if got := o.vaapiFilter(); got != "scale_vaapi=format=nv12" {
		t.Errorf("原分辨率滤镜不对: %s", got)
	}
}
