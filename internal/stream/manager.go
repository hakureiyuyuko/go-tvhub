// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tvhub/internal/store"
)

// ErrTooMany 表示并发转发路数已达上限。
var ErrTooMany = errors.New("服务器转发并发数已达上限，请稍后再试")

// Session 一个频道对应一个转发会话：一个 ffmpeg 进程 + 一个 HLS 分片目录。
// 多个观众共享同一个会话，不会重复拉源。
type Session struct {
	ChannelID   int64
	ChannelName string
	URL         string
	Kind        string

	dir     string
	headers string // 源站需要的自定义请求头
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	started time.Time
	last    atomic.Int64
	ready   atomic.Bool

	exitOnce sync.Once
	exited   chan struct{}
	exitMu   sync.Mutex
	exitErr  error

	logs *ringLog
	rate *rateTracker

	mu      sync.Mutex
	clients map[string]time.Time
}

func newSession(root string, ch store.Channel) *Session {
	return &Session{
		ChannelID:   ch.ID,
		ChannelName: ch.Name,
		URL:         ch.URL,
		Kind:        KindLabel(ch.URL),
		dir:         filepath.Join(root, fmt.Sprintf("ch%d-%d", ch.ID, time.Now().UnixNano())),
		exited:      make(chan struct{}),
		logs:        newRingLog(80),
		rate:        newRateTracker(10 * time.Second),
		clients:     map[string]time.Time{},
	}
}

func (s *Session) touch() { s.last.Store(time.Now().UnixNano()) }

// LastActive 返回最后一次被访问的时间。
func (s *Session) LastActive() time.Time {
	v := s.last.Load()
	if v == 0 {
		return s.started
	}
	return time.Unix(0, v)
}

// Dir 返回 HLS 分片目录。
func (s *Session) Dir() string { return s.dir }

// Ready 表示是否已经产出可播放的分片。
func (s *Session) Ready() bool { return s.ready.Load() }

// TouchClient 记录一次客户端访问（用于统计在线观看数）。
func (s *Session) TouchClient(key string) {
	s.touch()
	if key == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	s.clients[key] = now
	for k, t := range s.clients {
		if now.Sub(t) > 30*time.Second {
			delete(s.clients, k)
		}
	}
	s.mu.Unlock()
}

// Viewers 返回最近 30 秒内拉过播放列表的客户端数量。
func (s *Session) Viewers() int {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, t := range s.clients {
		if now.Sub(t) > 30*time.Second {
			delete(s.clients, k)
			continue
		}
		n++
	}
	return n
}

// start 启动 ffmpeg 转发进程。
func (s *Session) start(o Options) error {
	o = o.Normalize()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("创建分片目录失败: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	ch := store.Channel{ID: s.ChannelID, Name: s.ChannelName, URL: s.URL, Headers: s.headers}
	args := o.hlsArgs(ch, s.dir)

	cmd := exec.CommandContext(ctx, o.FFmpeg, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = s.logs
	// ffmpeg 的 -progress 行交给速率统计，其余（告警/报错）留在日志里
	s.logs.onLine = s.rate.feed
	s.cmd = cmd
	s.started = time.Now()
	s.touch()
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("启动 ffmpeg 失败（%s）: %w", o.FFmpeg, err)
	}
	go func() {
		err := cmd.Wait()
		s.exitMu.Lock()
		s.exitErr = err
		s.exitMu.Unlock()
		close(s.exited)
	}()
	return nil
}

func (s *Session) err() error {
	s.exitMu.Lock()
	defer s.exitMu.Unlock()
	return s.exitErr
}

// waitReady 等待出现第一个可播放的分片。
func (s *Session) waitReady(ctx context.Context, timeout time.Duration) error {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		if s.segmentCount() > 0 {
			s.ready.Store(true)
			s.touch()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.exited:
			return fmt.Errorf("转发进程已退出：%v%s", nonNil(s.err()), s.logSuffix())
		case <-deadline.C:
			return fmt.Errorf("等待直播流超时（%s）%s", timeout, s.logSuffix())
		case <-tick.C:
		}
	}
}

func nonNil(err error) string {
	if err == nil {
		return "被中断"
	}
	return err.Error()
}

func (s *Session) logSuffix() string {
	if l := s.logs.Tail(6); l != "" {
		return "\n" + l
	}
	return ""
}

// segmentCount 统计播放列表里已有的分片数。
func (s *Session) segmentCount() int {
	b, err := os.ReadFile(s.playlistPath())
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			n++
		}
	}
	return n
}

func (s *Session) playlistPath() string { return filepath.Join(s.dir, "index.m3u8") }

// Playlist 读取播放列表，尽量避开 ffmpeg 正在重写的中间状态。
func (s *Session) Playlist() ([]byte, error) {
	var last []byte
	for i := 0; i < 4; i++ {
		b, err := os.ReadFile(s.playlistPath())
		if err != nil {
			if last != nil {
				return last, nil
			}
			return nil, err
		}
		last = b
		if len(b) > 0 && bytes.HasSuffix(b, []byte("\n")) {
			return b, nil
		}
		time.Sleep(80 * time.Millisecond)
	}
	return last, nil
}

var segNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]*$`)

// SegmentPath 校验并返回分片文件路径。
func (s *Session) SegmentPath(name string) (string, error) {
	if !segNameRe.MatchString(name) || strings.Contains(name, "..") {
		return "", errors.New("非法分片名")
	}
	p := filepath.Join(s.dir, name)
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

// close 停止进程并清理分片目录。
func (s *Session) close() {
	if s.cancel != nil {
		s.cancel()
	}
	select {
	case <-s.exited:
	case <-time.After(3 * time.Second):
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		select {
		case <-s.exited:
		case <-time.After(2 * time.Second):
		}
	}
	_ = os.RemoveAll(s.dir)
}

// Stat 是会话对外暴露的状态。
type Stat struct {
	ChannelID int64  `json:"channel_id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	StartedAt string `json:"started_at"`
	UptimeSec int    `json:"uptime_sec"`
	IdleSec   int    `json:"idle_sec"`
	Viewers   int    `json:"viewers"`
	Error     string `json:"error,omitempty"`
	Log       string `json:"log,omitempty"`

	// 源站投递速率：媒体时间 / 真实时间。-1 表示暂无数据。
	// 1.00x 正常；明显小于 1 说明源站在限速或拥堵（画面会变慢/卡顿）。
	Rate float64 `json:"rate"`
	// 窗口内 mux 出的帧率及其峰值；与时间戳模式无关，可做交叉验证。
	FPS     float64 `json:"fps"`
	PeakFPS float64 `json:"peak_fps"`
}

// Stat 返回会话状态。
func (s *Session) Stat() Stat {
	st := Stat{
		ChannelID: s.ChannelID,
		Name:      s.ChannelName,
		Kind:      s.Kind,
		StartedAt: s.started.Format("2006-01-02 15:04:05"),
		UptimeSec: int(time.Since(s.started).Seconds()),
		IdleSec:   int(time.Since(s.LastActive()).Seconds()),
		Viewers:   s.Viewers(),
	}
	switch {
	case s.ready.Load():
		st.State = "ready"
	case s.isExited():
		st.State = "error"
		st.Error = nonNil(s.err())
	default:
		st.State = "starting"
	}
	if st.State == "error" {
		st.Log = s.logs.Tail(8)
	}
	if s.rate != nil {
		rate, fps, peak, ok := s.rate.snapshot(time.Now())
		if ok {
			st.Rate = round2(rate)
		}
		st.FPS = round1(fps)
		st.PeakFPS = round1(peak)
	}
	return st
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func (s *Session) isExited() bool {
	select {
	case <-s.exited:
		return true
	default:
		return false
	}
}

// Manager 管理全部转发会话。
type Manager struct {
	mu       sync.Mutex
	sessions map[int64]*Session
	root     string
	opts     func() Options
	log      *slog.Logger
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewManager 创建转发管理器，opts 每次取最新设置。
func NewManager(root string, opts func() Options, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		sessions: map[int64]*Session{},
		root:     root,
		opts:     opts,
		log:      log,
		stopCh:   make(chan struct{}),
	}
}

// Start 启动后台回收协程。
func (m *Manager) Start() {
	_ = os.RemoveAll(m.root)
	_ = os.MkdirAll(m.root, 0o755)
	go m.reapLoop()
}

// StopAll 停止全部会话（退出时调用）。
func (m *Manager) StopAll() {
	m.stopOnce.Do(func() { close(m.stopCh) })
	m.mu.Lock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.sessions = map[int64]*Session{}
	m.mu.Unlock()
	for _, s := range list {
		s.close()
	}
}

func (m *Manager) reapLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-t.C:
			m.reap()
		}
	}
}

func (m *Manager) reap() {
	idle := m.opts().Normalize().Idle
	now := time.Now()
	var victims []*Session
	m.mu.Lock()
	for id, s := range m.sessions {
		if s.isExited() {
			delete(m.sessions, id)
			victims = append(victims, s)
			continue
		}
		if now.Sub(s.LastActive()) > idle {
			delete(m.sessions, id)
			victims = append(victims, s)
		}
	}
	m.mu.Unlock()
	for _, s := range victims {
		m.log.Info("回收转发会话", "channel", s.ChannelID, "name", s.ChannelName)
		s.close()
	}
}

// Acquire 获取（必要时启动）某频道的转发会话。
func (m *Manager) Acquire(ctx context.Context, ch store.Channel) (*Session, error) {
	m.mu.Lock()
	if s, ok := m.sessions[ch.ID]; ok && !s.isExited() {
		s.touch()
		m.mu.Unlock()
		return s, nil
	}
	o := m.opts().Normalize()
	var victim *Session
	if len(m.sessions) >= o.Max {
		// 淘汰最久没人看的会话；如果全都在活跃观看，就拒绝新请求
		var oldest *Session
		for _, s := range m.sessions {
			if oldest == nil || s.LastActive().Before(oldest.LastActive()) {
				oldest = s
			}
		}
		if oldest == nil || time.Since(oldest.LastActive()) < 15*time.Second {
			m.mu.Unlock()
			return nil, ErrTooMany
		}
		delete(m.sessions, oldest.ChannelID)
		victim = oldest
	}
	s := newSession(m.root, ch)
	s.headers = ch.Headers
	s.touch()
	m.sessions[ch.ID] = s
	m.mu.Unlock()

	if victim != nil {
		m.log.Info("转发并发已满，替换最久未使用的频道", "channel", victim.ChannelID)
		victim.close()
	}
	if err := s.start(o); err != nil {
		m.drop(ch.ID, s)
		return nil, err
	}
	m.log.Info("启动转发", "channel", ch.ID, "name", ch.Name, "url", ch.URL)
	return s, nil
}

func (m *Manager) drop(id int64, s *Session) {
	m.mu.Lock()
	if cur, ok := m.sessions[id]; ok && cur == s {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
}

// Ready 启动并等待第一个分片就绪。
func (m *Manager) Ready(ctx context.Context, ch store.Channel, timeout time.Duration) (*Session, error) {
	s, err := m.Acquire(ctx, ch)
	if err != nil {
		return nil, err
	}
	if s.Ready() {
		return s, nil
	}
	if err := s.waitReady(ctx, timeout); err != nil {
		// 失败就丢弃会话，下次请求会重新拉起
		m.drop(ch.ID, s)
		s.close()
		return nil, err
	}
	return s, nil
}

// Get 返回已存在的会话。
func (m *Manager) Get(id int64) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// Stop 停止某频道的会话。
func (m *Manager) Stop(id int64) bool {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if ok {
		s.close()
	}
	return ok
}

// Stats 返回全部会话状态（按频道 ID 排序）。
func (m *Manager) Stats() []Stat {
	m.mu.Lock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.Unlock()
	out := make([]Stat, 0, len(list))
	for _, s := range list {
		out = append(out, s.Stat())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ChannelID < out[j].ChannelID })
	return out
}

// Active 返回当前会话数。
func (m *Manager) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// ringLog 是一个只保留最近若干行的 io.Writer，用来捕获 ffmpeg 的 stderr。
// onLine 非空时会先拿到每一行：返回 true 表示这行已被消费（例如 -progress
// 的进度行），不再进日志，避免把告警刷掉。
type ringLog struct {
	mu     sync.Mutex
	lines  []string
	cur    []byte
	max    int
	onLine func(string) bool
}

func newRingLog(max int) *ringLog { return &ringLog{max: max} }

func (r *ringLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cur = append(r.cur, p...)
	for {
		i := bytes.IndexByte(r.cur, '\n')
		if i < 0 {
			break
		}
		r.push(string(bytes.TrimRight(r.cur[:i], "\r")))
		r.cur = append([]byte(nil), r.cur[i+1:]...)
	}
	if len(r.cur) > 4096 {
		r.push(string(r.cur[:4096]))
		r.cur = nil
	}
	return len(p), nil
}

func (r *ringLog) push(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	if r.onLine != nil && r.onLine(line) {
		return
	}
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

// Tail 返回最后 n 行日志。
func (r *ringLog) Tail(n int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	lines := r.lines
	if len(r.cur) > 0 {
		lines = append(append([]string{}, lines...), strings.TrimSpace(string(r.cur)))
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
