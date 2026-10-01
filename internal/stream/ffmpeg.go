// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package stream

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"tvhub/internal/m3u"
	"tvhub/internal/store"
)

// probeTimeout 是单次 ffprobe 探测的超时（批量探测时也用它）。
const defaultProbeTimeout = 15 * time.Second

// 视频处理模式。
const (
	VideoModeAuto = "auto" // 自动（默认）：只有浏览器放不了的编码才转码
	VideoModeCopy = "copy" // 从不转码，全部直接复制
	VideoModeAll  = "all"  // 全部转码（调试或特殊源用）
)

// ClientCaps 是播放端声明的能力。
// 面板前端会告诉服务端它能不能直接放 HEVC（Safari / 电视盒子可以，Chrome/Firefox 不行）。
type ClientCaps struct {
	HEVC bool
}

// Options 是转发进程的运行时参数，由面板设置派生。
type Options struct {
	FFmpeg          string        // ffmpeg 可执行文件路径
	Transport       string        // RTSP 传输方式：tcp / udp
	AudioMode       string        // aac：音频转 AAC（浏览器兼容）；copy：音频直通
	VideoMode       string        // auto / copy / all
	TranscodeHeight int           // 转码输出高度，0 = 保持原分辨率
	TranscodeQP     int           // 转码 CQP 质量（越小越好）
	VAAPIDevice     string        // 显卡渲染节点，如 /dev/dri/renderD128
	SourceCodec     string        // 源站视频编码（会话探明后填入，auto 模式据此判断）
	ClientHEVC      bool          // 播放端声明能放 HEVC
	Sniff           bool          // 本次启动是为了探明源站编码（日志调到 info）
	HLSTime         int           // 分片时长（秒）
	HLSList         int           // 播放列表保留分片数
	Idle            time.Duration // 无观众后多久停止
	Max             int           // 最大并发转发路数
	Extra           string        // 追加的 ffmpeg 参数
	TSFix           bool          // 用到达时间重建单调时间轴（治源站时间戳跳跃）
	SplitByTime     bool          // 强制到点就切，不等关键帧
	ProbeTimeout    time.Duration // 单次 ffprobe 探测超时
}

// DefaultOptions 返回默认参数。
func DefaultOptions() Options {
	return Options{
		FFmpeg:          "ffmpeg",
		Transport:       "tcp",
		AudioMode:       "aac",
		VideoMode:       VideoModeAuto,
		TranscodeHeight: 1080,
		TranscodeQP:     26,
		VAAPIDevice:     "/dev/dri/renderD128",
		HLSTime:         2,
		HLSList:         6,
		Idle:            45 * time.Second,
		Max:             8,
		ProbeTimeout:    15 * time.Second,
	}
}

// Transcode 判断这一路是否要用显卡转码成 H.264。
// auto（默认）：源站是 H.264 就照旧直接复制；是 HEVC 这类浏览器放不了的编码才转码；
// 播放端自己声明能放 HEVC（Safari、电视盒子、VLC）时，HEVC 也不转。
func (o Options) Transcode(ch store.Channel) bool {
	if ch.Kind() == "http" { // HTTP 源走反向代理，不过 ffmpeg
		return false
	}
	switch o.VideoMode {
	case VideoModeAll:
		return true
	case VideoModeCopy:
		return false
	}
	codec := strings.ToLower(strings.TrimSpace(o.SourceCodec))
	if codec == "" { // 还没探明：先按复制起，Ready 会探完再决定要不要重来
		return false
	}
	if o.ClientHEVC && isHEVC(codec) {
		return false
	}
	return !browserPlayable(codec)
}

func isHEVC(codec string) bool {
	switch codec {
	case "hevc", "h265", "hvc1", "hev1":
		return true
	}
	return false
}

// browserPlayable 判断浏览器（MSE）能不能直接放这个编码。
// 实测 Chrome/Firefox 的 MSE 只靠得住 H.264；HEVC/AV1/MPEG-2/VC-1 等一律放不了，
// 所以白名单只留 H.264（多转一次总比黑屏好）。
func browserPlayable(codec string) bool {
	switch codec {
	case "h264", "avc1", "avc":
		return true
	}
	return false
}

// Normalize 修正非法取值。
func (o Options) Normalize() Options {
	if strings.TrimSpace(o.FFmpeg) == "" {
		o.FFmpeg = "ffmpeg"
	}
	if o.Transport != "tcp" && o.Transport != "udp" {
		o.Transport = "tcp"
	}
	if o.AudioMode != "copy" {
		o.AudioMode = "aac"
	}
	if o.HLSTime < 1 || o.HLSTime > 10 {
		o.HLSTime = 2
	}
	if o.HLSList < 3 || o.HLSList > 30 {
		o.HLSList = 6
	}
	if o.Idle < 10*time.Second {
		o.Idle = 10 * time.Second
	}
	if o.Max < 1 {
		o.Max = 1
	}
	if o.Max > 64 {
		o.Max = 64
	}
	if o.TranscodeHeight < 0 || o.TranscodeHeight > 4320 {
		o.TranscodeHeight = 1080
	}
	if o.TranscodeHeight > 0 && o.TranscodeHeight%2 != 0 { // H.264 要求偶数尺寸
		o.TranscodeHeight--
	}
	if o.TranscodeQP < 14 || o.TranscodeQP > 40 {
		o.TranscodeQP = 26
	}
	if o.VAAPIDevice == "" {
		o.VAAPIDevice = "/dev/dri/renderD128"
	}
	if o.ProbeTimeout < 3*time.Second || o.ProbeTimeout > 60*time.Second {
		o.ProbeTimeout = 15 * time.Second
	}
	if o.VideoMode == "vaapi" { // 兼容旧设置值
		o.VideoMode = VideoModeAll
	}
	if o.VideoMode != VideoModeAuto && o.VideoMode != VideoModeCopy && o.VideoMode != VideoModeAll {
		o.VideoMode = VideoModeAuto
	}
	return o
}

// FFprobe 返回与 ffmpeg 同目录的 ffprobe 路径。
func (o Options) FFprobe() string {
	if strings.TrimSpace(o.FFmpeg) == "" {
		return "ffprobe"
	}
	base := filepath.Base(o.FFmpeg)
	if base == "ffmpeg" || base == "ffmpeg.exe" {
		return strings.TrimSuffix(o.FFmpeg, base) + strings.Replace(base, "ffmpeg", "ffprobe", 1)
	}
	// 自定义路径：同目录下找 ffprobe
	dir := filepath.Dir(o.FFmpeg)
	return filepath.Join(dir, "ffprobe"+exeSuffix())
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// hlsArgs 构造 HLS 转发命令：默认视频流原样复制，auto 模式下源站是浏览器放不了的
// 编码（HEVC 等）时改成显卡硬件转码。
func (o Options) hlsArgs(ch store.Channel, outDir string) []string {
	o = o.Normalize()
	// -progress 输出（走 stderr）被 Session 用来统计「源站投递速率」，
	// 同时用 -nostats 关掉默认的统计行，避免刷屏。
	// 探编码（Sniff）时把日志提到 info，好从启动信息里读到 “Stream #0:0: Video: xxx”。
	level := "warning"
	if o.Sniff {
		level = "info"
	}
	args := []string{"-hide_banner", "-nostdin", "-loglevel", level, "-nostats", "-progress", "pipe:2"}
	switch ch.Kind() {
	case "rtsp":
		args = append(args, "-rtsp_transport", o.Transport, "-timeout", "15000000")
	case "rtmp":
		args = append(args, "-rtmp_live", "live")
	}
	// 源站的时间戳会周期性倒退（实测日志：Non-monotonic DTS + RTP bad cseq），
	// 后果是 HLS 分片时长变成 1 秒 / 十几秒混在一起，播放器时间轴跟着乱。
	// TSFix 用数据到达时间重建单调时间轴，能治这种情况（已做过 A/B 对比）。
	// 但它有个前提：源站得按实时投递。源站一旦限速/拥堵（实测遇到只有 0.3-0.5x
	// 实时的情况），到达时间就不再等于媒体时间，整路会变成慢放。所以默认关闭，
	// 只在确认源站时间戳坏掉、且源站投递正常时才打开。
	if o.TSFix && ch.Kind() != "http" {
		args = append(args, "-use_wallclock_as_timestamps", "1")
	}
	// 需要转码时：解码与编码都走显卡（4K HEVC 用软件解码在 2 核容器里拉不动）
	if o.Transcode(ch) {
		args = append(args, "-vaapi_device", o.VAAPIDevice,
			"-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi")
	}
	if h := strings.TrimSpace(ch.Headers); h != "" {
		args = append(args, "-headers", h)
	}
	args = append(args, splitArgs(o.Extra)...)
	args = append(args, "-i", ch.URL)
	// 只取第一条视频/音频，丢弃数据、字幕等轨道（IPTV 里常见，会拖坏 HLS）
	args = append(args, "-map", "0:v:0?", "-map", "0:a:0?", "-sn", "-dn")
	if o.Transcode(ch) {
		args = append(args, "-vf", o.vaapiFilter(), "-c:v", "h264_vaapi",
			"-rc_mode", "CQP", "-qp", strconv.Itoa(o.TranscodeQP))
	} else {
		args = append(args, "-c:v", "copy")
	}
	if o.AudioMode == "copy" {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-ac", "2", "-b:a", "192k")
	}
	return append(args,
		"-f", "hls",
		"-hls_time", strconv.Itoa(o.HLSTime),
		"-hls_list_size", strconv.Itoa(o.HLSList),
		"-hls_delete_threshold", "3",
		"-hls_flags", o.hlsFlags(),
		"-hls_segment_filename", filepath.Join(outDir, "seg_%05d.ts"),
		filepath.Join(outDir, "index.m3u8"),
	)
}

// hlsFlags 组装 HLS muxer 的 flags。
func (o Options) hlsFlags() string {
	f := "delete_segments+omit_endlist+independent_segments"
	if o.SplitByTime {
		// 不等待关键帧，到 hls_time 就切：源站关键帧间隔很长时用它把分片压回目标时长
		f += "+split_by_time"
	}
	return f
}

// vaapiFilter 构造 VAAPI 滤镜链：把帧转成编码器要的 nv12（10bit 源会在这里降到 8bit），
// 并按需等比缩放。这里不做 HDR 色调映射，HLG/PQ 源转出来颜色会偏灰。
func (o Options) vaapiFilter() string {
	h := o.TranscodeHeight
	if h <= 0 {
		return "scale_vaapi=format=nv12"
	}
	// w = iw*h/ih，向下取整到偶数（H.264 要求偶数尺寸）
	return fmt.Sprintf("scale_vaapi=w=trunc(iw*%d/ih/2)*2:h=%d:format=nv12", h, h)
}

// probeArgs 构造 ffprobe 命令。
func (o Options) probeArgs(ch store.Channel) []string {
	o = o.Normalize()
	args := []string{"-hide_banner", "-loglevel", "error"}
	if ch.Kind() == "rtsp" {
		args = append(args, "-rtsp_transport", o.Transport)
	}
	if h := strings.TrimSpace(ch.Headers); h != "" {
		args = append(args, "-headers", h)
	}
	return append(args,
		"-probesize", "2M", "-analyzeduration", "3000000",
		"-show_entries", "stream=codec_type,codec_name,width,height,avg_frame_rate,channels,sample_rate,bit_rate",
		"-show_entries", "format=format_name,bit_rate,duration",
		"-of", "json",
		"-i", ch.URL,
	)
}

// splitArgs 按空白拆分参数串，支持双引号包裹。
func splitArgs(s string) []string {
	var (
		out   []string
		cur   strings.Builder
		quote bool
	)
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			quote = !quote
		case (r == ' ' || r == '\t' || r == '\n') && !quote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// FFmpegVersion 检测 ffmpeg 是否可用，返回版本号。
func (o Options) FFmpegVersion() (string, error) {
	o = o.Normalize()
	ctx, cancel := timeoutCtx(defaultProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, o.FFmpeg, "-version").Output()
	if err != nil {
		return "", fmt.Errorf("无法执行 %s: %w", o.FFmpeg, err)
	}
	line := strings.SplitN(string(out), "\n", 2)[0]
	return strings.TrimSpace(line), nil
}

// ProbeResult 是 ffprobe 的摘要信息。
type ProbeResult struct {
	Summary string `json:"summary"`
	Codec   string `json:"codec"` // h264 / hevc ...
	Audio   string `json:"audio"` // aac / mp2 ...
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	FPS     string `json:"fps"`
	Bitrate int    `json:"bitrate"` // bps
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

type ffprobeJSON struct {
	Streams []struct {
		CodecType    string `json:"codec_type"`
		CodecName    string `json:"codec_name"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		AvgFrameRate string `json:"avg_frame_rate"`
		Channels     int    `json:"channels"`
		SampleRate   string `json:"sample_rate"`
		BitRate      string `json:"bit_rate"`
	} `json:"streams"`
	Format struct {
		FormatName string `json:"format_name"`
		BitRate    string `json:"bit_rate"`
		Duration   string `json:"duration"`
	} `json:"format"`
}

// Probe 用 ffprobe 探测频道，用于面板排障。
func (o Options) Probe(ch store.Channel) ProbeResult {
	o = o.Normalize()
	ctx, cancel := timeoutCtx(o.ProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, o.FFprobe(), o.probeArgs(ch)...)
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		if msg == "" {
			msg = err.Error()
		}
		return ProbeResult{OK: false, Error: firstLines(msg, 3)}
	}
	var p ffprobeJSON
	if err := json.Unmarshal(out, &p); err != nil {
		return ProbeResult{OK: false, Error: "解析 ffprobe 输出失败: " + err.Error()}
	}
	res := ProbeResult{OK: true}
	var audio []string
	for _, s := range p.Streams {
		switch s.CodecType {
		case "video":
			if res.Codec == "" {
				res.Codec = s.CodecName
				res.Width, res.Height = s.Width, s.Height
				res.FPS = simplifyFPS(s.AvgFrameRate)
			}
		case "audio":
			if len(audio) == 0 {
				res.Audio = s.CodecName
				desc := s.CodecName
				if s.Channels > 0 {
					desc += fmt.Sprintf(" %dch", s.Channels)
				}
				if s.SampleRate != "" {
					desc += " " + s.SampleRate + "Hz"
				}
				audio = append(audio, desc)
			}
		}
	}
	if n, err := strconv.Atoi(p.Format.BitRate); err == nil {
		res.Bitrate = n
	}
	parts := make([]string, 0, 3)
	if res.Codec != "" {
		v := res.Codec
		if res.Width > 0 {
			v += fmt.Sprintf(" %dx%d", res.Width, res.Height)
		}
		if res.FPS != "" {
			v += " " + res.FPS
		}
		parts = append(parts, v)
	}
	if len(audio) > 0 {
		parts = append(parts, audio[0])
	}
	if res.Bitrate > 0 {
		parts = append(parts, fmt.Sprintf("%.1f Mbps", float64(res.Bitrate)/1e6))
	}
	res.Summary = strings.Join(parts, " · ")
	if res.Summary == "" {
		res.Summary = "未识别到音视频轨道"
	}
	return res
}

func simplifyFPS(rate string) string {
	num, den, ok := strings.Cut(rate, "/")
	if !ok {
		return rate
	}
	n, err1 := strconv.Atoi(num)
	d, err2 := strconv.Atoi(den)
	if err1 != nil || err2 != nil || d == 0 {
		return rate
	}
	v := float64(n) / float64(d)
	if v == float64(int(v)) {
		return fmt.Sprintf("%dfps", int(v))
	}
	return fmt.Sprintf("%.2ffps", v)
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " / ")
}

// KindLabel 返回协议的中文标签。
func KindLabel(u string) string {
	switch m3u.Kind(u) {
	case "rtsp":
		return "RTSP"
	case "rtmp":
		return "RTMP"
	case "http":
		return "HTTP"
	case "udp":
		return "UDP"
	}
	return "其他"
}
