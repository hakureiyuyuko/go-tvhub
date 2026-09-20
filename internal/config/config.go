// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

// Package config 把数据库里的设置项映射成运行时配置。
package config

import (
	"strconv"
	"strings"
	"time"

	"tvhub/internal/store"
	"tvhub/internal/stream"
)

// 设置项键名。
const (
	KeySiteTitle     = "site_title"
	KeyFFmpegPath    = "ffmpeg_path"
	KeyTransport     = "rtsp_transport"
	KeyAudioMode     = "audio_mode"
	KeyHLSTime       = "hls_time"
	KeyHLSListSize   = "hls_list_size"
	KeyIdleSeconds   = "idle_seconds"
	KeyMaxSessions   = "max_sessions"
	KeyExtraArgs     = "extra_ffmpeg_args"
	KeyProbeTimeout  = "probe_timeout"
	KeyProbeInterval = "probe_interval_ms"
	KeyM3UContent    = "m3u_content"
	KeyM3USource     = "m3u_source_name"
	KeyM3UApplied    = "m3u_applied_at"
	KeySecret        = "server_secret"

	KeyTurnstileEnabled = "turnstile_enabled"
	KeyTurnstileSiteKey = "turnstile_site_key"
	KeyTurnstileSecret  = "turnstile_secret_key"
)

// Field 描述一个可在面板里编辑的设置项。
type Field struct {
	Key     string
	Label   string
	Hint    string
	Type    string // text | password | int | select | bool
	Options []Opt
	Default string
	ShowIf  string // 依赖的开关（bool）设置项：仅当它为真时才显示本项
}

// Opt 是下拉选项。
type Opt struct{ Value, Label string }

// Fields 返回设置界面的字段定义。
func Fields() []Field {
	return []Field{
		{Key: KeySiteTitle, Label: "站点标题", Type: "text", Default: "家庭电视", Hint: "显示在页面标题和播放器顶部"},
		{Key: KeyFFmpegPath, Label: "ffmpeg 路径", Type: "text", Default: "ffmpeg", Hint: "留空或 ffmpeg 表示使用 PATH 中的版本；填绝对路径可指定自编译版本"},
		{Key: KeyTransport, Label: "RTSP 传输方式", Type: "select", Default: "tcp", Options: []Opt{{"tcp", "TCP（推荐，稳定）"}, {"udp", "UDP（低延迟）"}}, Hint: "源站对 UDP 丢包敏感时选 TCP"},
		{Key: KeyAudioMode, Label: "音频处理", Type: "select", Default: "aac", Options: []Opt{{"aac", "转成 AAC（浏览器兼容，推荐）"}, {"copy", "原样复制（零转码，但部分浏览器无法播放 MP2/AC3）"}}, Hint: "视频一律原样复制，不转码"},
		{Key: KeyHLSTime, Label: "分片时长（秒）", Type: "int", Default: "2", Hint: "越小延迟越低，但请求更频繁；建议 2-4"},
		{Key: KeyHLSListSize, Label: "播放列表分片数", Type: "int", Default: "6", Hint: "决定播放端的缓冲时长"},
		{Key: KeyIdleSeconds, Label: "无人观看后停止（秒）", Type: "int", Default: "45", Hint: "多久没有请求就关掉转发进程"},
		{Key: KeyMaxSessions, Label: "最大并发转发路数", Type: "int", Default: "8", Hint: "每路约消耗一个 CPU 核的带宽与少量运算；家用建议 4-8"},
		{Key: KeyExtraArgs, Label: "追加 ffmpeg 参数", Type: "text", Default: "", Hint: "高级选项，会插到 -i 之前，例如 -rtsp_flags prefer_tcp"},
		{Key: KeyProbeTimeout, Label: "探测超时（秒）", Type: "int", Default: "15",
			Hint: "源站不给你看的频道会一直挂着不响应，所以这个值直接决定「一键探测」跑多快；能正常播的频道通常 1 秒内就返回，可以调到 5-8 秒"},
		{Key: KeyProbeInterval, Label: "探测间隔（毫秒）", Type: "int", Default: "1500",
			Hint: "两次探测之间的间隔。IPTV 平台是按账号限流的（触发后 1 小时内面板和机顶盒都看不了），被限流过就调到 5000-8000 更保险，代价是跑得更慢"},
		{Key: KeyTurnstileEnabled, Label: "登录页启用 Cloudflare Turnstile 人机验证", Type: "bool", Default: "0",
			Hint: "防止暴力破解和爬虫。注意：开启后登录页需要能访问 challenges.cloudflare.com，纯内网环境会无法登录"},
		{Key: KeyTurnstileSiteKey, Label: "Turnstile 站点密钥（Site Key）", Type: "text", ShowIf: KeyTurnstileEnabled,
			Hint: "Cloudflare 控制台 → Turnstile → 添加站点，得到的 Site Key（可以公开）"},
		{Key: KeyTurnstileSecret, Label: "Turnstile 密钥（Secret Key）", Type: "password", ShowIf: KeyTurnstileEnabled,
			Hint: "同一页面上的 Secret Key，只保存在本机数据库，不要泄露"},
	}
}

func defaults() map[string]string {
	m := map[string]string{}
	for _, f := range Fields() {
		m[f.Key] = f.Default
	}
	return m
}

// Values 是合并默认值后的设置集合。
type Values struct {
	m map[string]string
}

// Load 从数据库读取设置。
func Load(st *store.Store) *Values {
	all, err := st.AllSettings()
	if err != nil {
		all = map[string]string{}
	}
	m := defaults()
	for k, v := range all {
		m[k] = v
	}
	return &Values{m: m}
}

// Get 返回字符串设置。
func (v *Values) Get(key string) string { return v.m[key] }

// GetDefault 返回默认值。
func (v *Values) GetDefault(key string) string {
	for _, f := range Fields() {
		if f.Key == key {
			return f.Default
		}
	}
	return ""
}

// Int 返回整数设置。
func (v *Values) Int(key string, def int) int {
	s := strings.TrimSpace(v.m[key])
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// SiteTitle 返回站点标题。
func (v *Values) SiteTitle() string {
	if s := strings.TrimSpace(v.m[KeySiteTitle]); s != "" {
		return s
	}
	return "家庭电视"
}

// Bool 返回布尔设置（1/true/on/yes 视为真）。
func (v *Values) Bool(key string, def bool) bool {
	s := strings.ToLower(strings.TrimSpace(v.m[key]))
	if s == "" {
		return def
	}
	switch s {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// TurnstileEnabled 是否勾选了登录页人机验证。
func (v *Values) TurnstileEnabled() bool { return v.Bool(KeyTurnstileEnabled, false) }

// TurnstileSiteKey 返回站点密钥。
func (v *Values) TurnstileSiteKey() string { return strings.TrimSpace(v.m[KeyTurnstileSiteKey]) }

// TurnstileSecret 返回密钥。
func (v *Values) TurnstileSecret() string { return strings.TrimSpace(v.m[KeyTurnstileSecret]) }

// TurnstileActive 表示人机验证会真正生效（已启用且两个密钥都填了）。
// 没填密钥时故意不生效，避免把自己锁在门外。
func (v *Values) TurnstileActive() bool {
	return v.TurnstileEnabled() && v.TurnstileSiteKey() != "" && v.TurnstileSecret() != ""
}

// StreamOptions 生成转发进程参数。
func (v *Values) StreamOptions() stream.Options {
	o := stream.DefaultOptions()
	o.FFmpeg = strings.TrimSpace(v.m[KeyFFmpegPath])
	if o.FFmpeg == "" {
		o.FFmpeg = "ffmpeg"
	}
	o.Transport = strings.ToLower(strings.TrimSpace(v.m[KeyTransport]))
	o.AudioMode = strings.ToLower(strings.TrimSpace(v.m[KeyAudioMode]))
	o.HLSTime = v.Int(KeyHLSTime, 2)
	o.HLSList = v.Int(KeyHLSListSize, 6)
	o.Idle = time.Duration(v.Int(KeyIdleSeconds, 45)) * time.Second
	o.Max = v.Int(KeyMaxSessions, 8)
	o.Extra = v.m[KeyExtraArgs]
	o.ProbeTimeout = time.Duration(v.Int(KeyProbeTimeout, 15)) * time.Second
	return o.Normalize()
}
