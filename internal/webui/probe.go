// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package webui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tvhub/internal/config"
	"tvhub/internal/store"
)

const (
	// 批量探测的并发数。故意设成 1（完全串行）：
	//
	// 实测教训（2026-09）：并发 6 时源站报过 "562 (Wait MLSS TimeOut)"；
	// 短时间内跑了几轮全量探测（约 250 次会话/40 分钟）之后，IPTV 平台直接对
	// 这个账号返回 429 RateLimitedExceeded: please try again in 1 hour，
	// 连机顶盒都一起看不了。因为探测用的是跟机顶盒同一套 AuthInfo（同一个账号额度）。
	// 宁可跑得慢，也别再把用户家里的电视搞挂。
	probeConcurrency = 1
	// 每个探测之间的默认间隔（毫秒级配置项 probe_interval_ms 控制）
	probeDefaultInterval = 1500 * time.Millisecond
	probeMinInterval     = 200 * time.Millisecond
	probeMaxInterval     = 10 * time.Second
	// 最多保留多少个失败频道名给前端展示
	probeMaxFailedNames = 60
	// 两轮探测之间的硬性冷却：触发限流的代价是「一小时内面板和机顶盒都看不了电视」，
	// 所以宁可拦住重复点击。
	probeCooldown = 5 * time.Minute
)

// ProbeStatus 是一次批量探测的进度快照（直接序列化给前端轮询）。
type ProbeStatus struct {
	Running     bool     `json:"running"`
	Total       int      `json:"total"`
	Done        int      `json:"done"`
	OK          int      `json:"ok"`
	Failed      int      `json:"failed"`
	Disabled    int      `json:"disabled"`
	Current     string   `json:"current"`
	StartedAt   string   `json:"started_at"`
	FinishedAt  string   `json:"finished_at"`
	AutoDisable bool     `json:"auto_disable"`
	Cancelled   bool     `json:"cancelled"`
	FailedNames []string `json:"failed_names"`
}

// probeJob 管理「一键探测」后台任务，同一时刻只跑一个。
type probeJob struct {
	mu         sync.Mutex
	st         ProbeStatus
	cancel     atomic.Bool
	lastFinish time.Time
}

// Status 返回当前进度。
func (p *probeJob) Status() ProbeStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.st
	out.FailedNames = append([]string(nil), p.st.FailedNames...)
	return out
}

func (p *probeJob) set(fn func(*ProbeStatus)) {
	p.mu.Lock()
	fn(&p.st)
	p.mu.Unlock()
}

// Cancel 请求停止后续探测（正在跑的那几个会自然结束）。
func (p *probeJob) Cancel() {
	p.cancel.Store(true)
}

// startProbe 启动批量探测；已有任务在跑时返回错误。
func (s *Server) startProbe(autoDisable, includeDisabled bool) (ProbeStatus, error) {
	if s.probe == nil {
		return ProbeStatus{}, errors.New("探测任务未初始化")
	}
	s.probe.mu.Lock()
	if s.probe.st.Running {
		st := s.probe.st
		s.probe.mu.Unlock()
		return st, errors.New("已有探测任务在进行中")
	}
	if !s.probe.lastFinish.IsZero() {
		if since := time.Since(s.probe.lastFinish); since < probeCooldown {
			wait := int((probeCooldown - since).Minutes()) + 1
			s.probe.mu.Unlock()
			return s.probe.st, fmt.Errorf("上一轮探测 %d 分钟前刚结束，为避免触发源站限流（触发后连机顶盒都会一起看不了，要等 1 小时），请 %d 分钟后再试", int(since.Minutes()), wait)
		}
	}
	s.probe.cancel.Store(false)
	s.probe.st = ProbeStatus{
		Running:     true,
		AutoDisable: autoDisable,
		StartedAt:   time.Now().Format("15:04:05"),
	}
	s.probe.mu.Unlock()

	list, err := s.st.ListChannels(includeDisabled)
	if err != nil {
		s.probe.set(func(st *ProbeStatus) { st.Running = false })
		return ProbeStatus{}, err
	}
	// 只探测有音视频源可以读的频道
	targets := make([]store.Channel, 0, len(list))
	for _, c := range list {
		if c.Kind() != "other" {
			targets = append(targets, c)
		}
	}
	s.probe.set(func(st *ProbeStatus) { st.Total = len(targets) })

	// 间隔可通过设置项调整：被限流过的话就调大，把请求频率压下来
	interval := time.Duration(s.Config().Int(config.KeyProbeInterval, int(probeDefaultInterval/time.Millisecond))) * time.Millisecond
	if interval < probeMinInterval || interval > probeMaxInterval {
		interval = probeDefaultInterval
	}
	go s.runProbe(targets, autoDisable, interval)
	return s.probe.Status(), nil
}

func (s *Server) runProbe(targets []store.Channel, autoDisable bool, interval time.Duration) {
	sem := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	for _, ch := range targets {
		if s.probe.cancel.Load() {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(ch store.Channel) {
			defer wg.Done()
			defer func() { <-sem }()
			s.probe.set(func(st *ProbeStatus) { st.Current = ch.Name })

			res := s.Config().StreamOptions().Probe(ch)
			text := res.Summary
			if !res.OK {
				text = "探测失败：" + res.Error
			}
			if err := s.st.SetChannelProbe(ch.ID, text); err != nil {
				s.log.Warn("保存探测结果失败", "channel", ch.ID, "err", err)
			}
			s.probe.set(func(st *ProbeStatus) {
				st.Done++
				if res.OK {
					st.OK++
				} else {
					st.Failed++
					if len(st.FailedNames) < probeMaxFailedNames {
						st.FailedNames = append(st.FailedNames, ch.Name)
					}
				}
			})
			if !res.OK && autoDisable {
				if err := s.st.SetChannelDisabled(ch.ID, true); err == nil {
					s.mgr.Stop(ch.ID)
					s.probe.set(func(st *ProbeStatus) { st.Disabled++ })
				}
			}
		}(ch)
		time.Sleep(interval) // 错开启动，把请求频率压到接近“人手动换台”的水平
	}
	wg.Wait()

	cancelled := s.probe.cancel.Load()
	s.probe.set(func(st *ProbeStatus) {
		st.Running = false
		st.Current = ""
		st.Cancelled = cancelled
		st.FinishedAt = time.Now().Format("15:04:05")
	})
	s.probe.mu.Lock()
	s.probe.lastFinish = time.Now()
	s.probe.mu.Unlock()
	final := s.probe.Status()
	s.log.Info("批量探测完成", "总", final.Total, "可用", final.OK, "失败", final.Failed,
		"自动停用", final.Disabled, "取消", final.Cancelled)
}

// isProbeFailed 判断频道是否被标记为探测失败。
func isProbeFailed(probe string) bool {
	return strings.HasPrefix(strings.TrimSpace(probe), "探测失败")
}

// disableFailedChannels 把所有「已被标记为探测失败」的频道停用，返回停用数量。
func (s *Server) disableFailedChannels(ctx context.Context) int {
	list, err := s.st.ListChannels(true)
	if err != nil {
		return 0
	}
	n := 0
	for _, c := range list {
		if c.Disabled || !isProbeFailed(c.Probe) {
			continue
		}
		if err := s.st.SetChannelDisabled(c.ID, true); err == nil {
			s.mgr.Stop(c.ID)
			n++
		}
		select {
		case <-ctx.Done():
			return n
		default:
		}
	}
	return n
}
