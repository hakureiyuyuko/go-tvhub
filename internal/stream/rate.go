// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package stream

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// rateTracker 从 ffmpeg 的 -progress 输出里算「源站投递速率」：
//
//	速率 = 窗口内媒体时间推进 / 窗口内真实时间
//	帧率 = 窗口内 mux 出的帧数 / 窗口内真实时间
//
// 1.00x 表示源站按实时投递；0.50x 表示源站只给了一半的数据，此时播放端无论
// 怎么配都会变慢或卡顿（源站侧限速/拥堵，面板补不出来）。
//
// 注意：开启「时间戳修复」（-use_wallclock_as_timestamps）后，输出时间轴按数据
// 到达时间重建，速率会恒等于 1.00x；此时看帧率（相对本会话峰值）仍能反出真实投递量。
type rateTracker struct {
	mu      sync.Mutex
	window  time.Duration
	samples []rateSample
	cur     rateSample
	haveCur bool
	peakFPS float64 // 本会话内见过的最高窗口帧率
}

type rateSample struct {
	at     time.Time
	media  time.Duration // ffmpeg 输出时间轴（= 已写出的媒体时间）
	frames int64         // 已 mux 出的帧数
}

// rateStale 超过这么久没有新的进度输出，就认为投递已经停住（速率按 0 处理）。
const rateStale = 6 * time.Second

// progressKeys 是 ffmpeg -progress 会输出的字段；这些行只用于统计，不进日志。
var progressKeys = map[string]bool{
	"frame": true, "fps": true, "bitrate": true, "total_size": true,
	"out_time_us": true, "out_time_ms": true, "out_time": true,
	"dup_frames": true, "drop_frames": true, "speed": true, "progress": true,
}

func newRateTracker(window time.Duration) *rateTracker {
	if window <= 0 {
		window = 10 * time.Second
	}
	return &rateTracker{window: window}
}

// feed 处理一行 ffmpeg 输出；返回 true 表示这是进度行（已消费，不进日志）。
func (t *rateTracker) feed(line string) bool {
	line = strings.TrimSpace(line)
	key, val, ok := strings.Cut(line, "=")
	if !ok {
		return false
	}
	if strings.HasPrefix(key, "stream_") { // stream_0_0_q=-1.0
		return true
	}
	if !progressKeys[key] {
		return false
	}
	val = strings.TrimSpace(val)
	switch key {
	case "out_time_us":
		// 顺带说明：ffmpeg 的 out_time_ms 字段实际也是微秒（历史遗留），这里只用 us。
		if us, err := strconv.ParseInt(val, 10, 64); err == nil && us >= 0 {
			t.mu.Lock()
			t.cur.media = time.Duration(us) * time.Microsecond
			t.haveCur = true
			t.mu.Unlock()
		}
	case "frame":
		if n, err := strconv.ParseInt(val, 10, 64); err == nil && n >= 0 {
			t.mu.Lock()
			t.cur.frames = n
			t.haveCur = true
			t.mu.Unlock()
		}
	case "progress":
		if val == "continue" || val == "end" {
			t.push(time.Now())
		}
	}
	return true
}

func (t *rateTracker) push(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.haveCur {
		return
	}
	t.samples = append(t.samples, rateSample{at: now, media: t.cur.media, frames: t.cur.frames})
	t.trim(now)
	if _, fps, ok := t.windowLocked(); ok && fps > t.peakFPS {
		t.peakFPS = fps
	}
}

// trim 丢掉已经滑出窗口的样本（至少保留一条，用作起点）。
func (t *rateTracker) trim(now time.Time) {
	cut := now.Add(-t.window - 2*time.Second)
	i := 0
	for i < len(t.samples)-1 && t.samples[i].at.Before(cut) {
		i++
	}
	if i > 0 {
		t.samples = append([]rateSample(nil), t.samples[i:]...)
	}
}

// windowLocked 用窗口内的样本算速率与帧率。
func (t *rateTracker) windowLocked() (rate, fps float64, ok bool) {
	if len(t.samples) < 2 {
		return 0, 0, false
	}
	last := t.samples[len(t.samples)-1]
	first := t.samples[0]
	from := last.at.Add(-t.window)
	for _, s := range t.samples {
		if !s.at.Before(from) {
			first = s
			break
		}
	}
	dt := last.at.Sub(first.at).Seconds()
	if dt < 2 { // 样本太少，先不给结论
		return 0, 0, false
	}
	rate = float64(last.media-first.media) / float64(time.Second) / dt
	fps = float64(last.frames-first.frames) / dt
	return rate, fps, true
}

// snapshot 返回当前速率、窗口帧率与本会话峰值帧率。
// rate 为负表示「暂无数据」（刚起流、刚停或进程卡住）。
func (t *rateTracker) snapshot(now time.Time) (rate, fps, peakFPS float64, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.trim(now)
	peakFPS = t.peakFPS
	if len(t.samples) < 2 || now.Sub(t.samples[len(t.samples)-1].at) > rateStale {
		return -1, 0, peakFPS, false
	}
	rate, fps, ok = t.windowLocked()
	if !ok {
		return -1, 0, peakFPS, false
	}
	return rate, fps, peakFPS, true
}
