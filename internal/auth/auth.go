// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

// Package auth 负责密码哈希、登录会话与登录限流。
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"tvhub/internal/store"
)

// CookieName 是面板登录 Cookie 名。
const CookieName = "tvhub_session"

// SessionTTL 是登录会话有效期。
const SessionTTL = 7 * 24 * time.Hour

var (
	// ErrBadCredentials 用户名或密码错误。
	ErrBadCredentials = errors.New("用户名或密码错误")
	// ErrDisabled 账号已停用。
	ErrDisabled = errors.New("账号已被停用")
	// ErrLocked 登录尝试过于频繁。
	ErrLocked = errors.New("登录尝试过于频繁")
)

// Manager 管理认证相关逻辑。
type Manager struct {
	st     *store.Store
	secure bool
	lim    *limiter
}

// New 创建认证管理器。
func New(st *store.Store) *Manager {
	return &Manager{st: st, lim: newLimiter()}
}

// SetSecureCookie 设置 Cookie 是否带 Secure 标记（使用 HTTPS 时打开）。
func (m *Manager) SetSecureCookie(v bool) { m.secure = v }

// Hash 生成 bcrypt 密码哈希。
func (m *Manager) Hash(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(b), err
}

// NewToken 生成 n 字节随机令牌的十六进制字符串。
func NewToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand 不可用: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// Authenticate 校验用户名密码，成功返回用户。
func (m *Manager) Authenticate(username, password, ip string) (*store.User, error) {
	key := strings.ToLower(strings.TrimSpace(username)) + "|" + ip
	if wait, blocked := m.lim.blocked(key); blocked {
		return nil, fmt.Errorf("%w，请 %s 后再试", ErrLocked, humanWait(wait))
	}
	u, err := m.st.UserByUsername(strings.TrimSpace(username))
	if err != nil {
		m.lim.fail(key)
		return nil, ErrBadCredentials
	}
	if !u.Enabled {
		return nil, ErrDisabled
	}
	hash, err := m.st.PasswordHash(u.ID)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		m.lim.fail(key)
		return nil, ErrBadCredentials
	}
	m.lim.reset(key)
	return u, nil
}

// StartSession 为用户创建会话并下发 Cookie。
func (m *Manager) StartSession(w http.ResponseWriter, r *http.Request, u *store.User) error {
	token := NewToken(32)
	expires := time.Now().Add(SessionTTL)
	if err := m.st.CreateSession(token, u.ID, expires, ClientIP(r), truncate(r.UserAgent(), 200)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(SessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   m.secure,
	})
	return nil
}

// EndSession 注销当前会话。
func (m *Manager) EndSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		_ = m.st.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: m.secure,
	})
}

// UserFromRequest 从 Cookie 解析当前登录用户，未登录返回 nil。
func (m *Manager) UserFromRequest(r *http.Request) *store.User {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	u, err := m.st.SessionUser(c.Value)
	if err != nil {
		return nil
	}
	return u
}

// UserFromStreamToken 用播放令牌鉴权（外部播放器 / 电视盒子用），支持
// 查询参数 token、路径或 X-Stream-Token 头。
func (m *Manager) UserFromStreamToken(token string) *store.User {
	if token == "" {
		return nil
	}
	u, err := m.st.UserByStreamToken(token)
	if err != nil || !u.Enabled {
		return nil
	}
	return u
}

// ClientIP 取客户端 IP（优先 X-Forwarded-For 的第一段）。
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		return strings.TrimSpace(xff)
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return strings.TrimSpace(xr)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func humanWait(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds())+1)
	}
	return fmt.Sprintf("%d 分钟", int(d.Minutes())+1)
}

const (
	limWindow = 15 * time.Minute
	limMax    = 10
	limBlock  = 10 * time.Minute
)

type failState struct {
	count int
	first time.Time
	until time.Time
}

type limiter struct {
	mu sync.Mutex
	m  map[string]*failState
}

func newLimiter() *limiter { return &limiter{m: map[string]*failState{}} }

func (l *limiter) blocked(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.m[key]
	if !ok {
		return 0, false
	}
	now := time.Now()
	if now.Before(st.until) {
		return st.until.Sub(now), true
	}
	if now.Sub(st.first) > limWindow {
		delete(l.m, key)
	}
	return 0, false
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	st, ok := l.m[key]
	if !ok || now.Sub(st.first) > limWindow {
		st = &failState{first: now}
		l.m[key] = st
	}
	st.count++
	if st.count >= limMax {
		st.until = now.Add(limBlock)
		st.count = 0
		st.first = now
	}
	if len(l.m) > 4096 { // 简单防膨胀
		for k, v := range l.m {
			if now.Sub(v.first) > limWindow && now.After(v.until) {
				delete(l.m, k)
			}
		}
	}
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, key)
}
