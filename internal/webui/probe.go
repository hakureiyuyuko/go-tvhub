// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package webui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tvhub/internal/store"
)

const (
	// 批量探测的并发数：别一次给源站开太多会话
	probeConcurrency = 6
	// 每次探测之间错开一下，避免「同一个瞬间」给源站开多路会话
	// （有的 IPTV 服务器会话数一多就直接返回 562 繁忙）
	probeStagger = 200 * time.Millisecond
	// 最多保留多少个失败频道名给前端展示
	probeMaxFailedNames = 60
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
	mu     sync.Mutex
	st     ProbeStatus
	cancel atomic.Bool
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
	go s.runProbe(targets, autoDisable)
	return s.probe.Status(), nil
}

func (s *Server) runProbe(targets []store.Channel, autoDisable bool) {
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
		time.Sleep(probeStagger) // 错开启动，别让源站同时收到一堆 RTSP 会话
	}
	wg.Wait()

	cancelled := s.probe.cancel.Load()
	s.probe.set(func(st *ProbeStatus) {
		st.Running = false
		st.Current = ""
		st.Cancelled = cancelled
		st.FinishedAt = time.Now().Format("15:04:05")
	})
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
