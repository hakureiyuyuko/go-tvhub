// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

// Package webui 提供面板页面、JSON 接口与流转发入口。
package webui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"tvhub/internal/auth"
	"tvhub/internal/config"
	"tvhub/internal/store"
	"tvhub/internal/stream"
)

//go:embed templates/*.html static/*
var assets embed.FS

type ctxKey int

const ctxUser ctxKey = 0

// Server 是 HTTP 层的依赖集合。
type Server struct {
	st      *store.Store
	auth    *auth.Manager
	mgr     *stream.Manager
	proxy   *stream.HTTPProxy
	tpl     map[string]*template.Template
	static  http.Handler
	log     *slog.Logger
	version string
	baseURL string
	started time.Time

	cfgMu sync.Mutex
	cfgv  *config.Values
	cfgAt time.Time

	ffMu  sync.Mutex
	ffVer string
	ffErr error
	ffAt  time.Time

	probe *probeJob
}

// New 创建 HTTP 服务。
func New(st *store.Store, am *auth.Manager, mgr *stream.Manager, prx *stream.HTTPProxy, version, baseURL string, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		st: st, auth: am, mgr: mgr, proxy: prx,
		log: log, version: version, baseURL: strings.TrimRight(baseURL, "/"),
		started: time.Now(),
		tpl:     map[string]*template.Template{},
		probe:   &probeJob{},
	}
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	s.static = http.FileServer(http.FS(sub))
	for _, page := range []string{"login.html", "player.html", "admin.html"} {
		t, err := template.New("layout").Funcs(templateFuncs()).ParseFS(assets, "templates/layout.html", "templates/"+page)
		if err != nil {
			return nil, err
		}
		s.tpl[strings.TrimSuffix(page, ".html")] = t
	}
	return s, nil
}

// Config 返回带缓存的设置（写入后调用 InvalidateConfig 立即失效）。
func (s *Server) Config() *config.Values {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if s.cfgv == nil || time.Since(s.cfgAt) > 3*time.Second {
		s.cfgv = config.Load(s.st)
		s.cfgAt = time.Now()
	}
	return s.cfgv
}

// InvalidateConfig 让设置缓存立即失效。
func (s *Server) InvalidateConfig() {
	s.cfgMu.Lock()
	s.cfgv = nil
	s.cfgMu.Unlock()
}

// Handler 组装全部路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", s.static))

	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLoginPost)
	mux.HandleFunc("GET /logout", s.handleLogout)
	mux.HandleFunc("POST /logout", s.handleLogout)

	mux.Handle("GET /{$}", s.needUser(s.handlePlayer))
	mux.Handle("GET /admin", s.needAdmin(s.handleAdminPage))

	mux.Handle("GET /api/channels", s.needUser(s.apiChannels))
	mux.Handle("POST /api/favorite/{id}", s.needUser(s.apiFavorite))
	mux.Handle("POST /api/stream/{id}/prepare", s.needUser(s.apiPrepare))
	mux.Handle("GET /api/stream/{id}/status", s.needUser(s.apiStatus))
	mux.Handle("POST /api/stream/{id}/stop", s.needUser(s.apiStopStream))

	mux.Handle("GET /admin/api/overview", s.needAdmin(s.apiOverview))
	mux.Handle("GET /admin/api/users", s.needAdmin(s.apiUsers))
	mux.Handle("POST /admin/api/users", s.needAdmin(s.apiUserCreate))
	mux.Handle("POST /admin/api/users/{id}/password", s.needAdmin(s.apiUserPassword))
	mux.Handle("POST /admin/api/users/{id}/role", s.needAdmin(s.apiUserRole))
	mux.Handle("POST /admin/api/users/{id}/toggle", s.needAdmin(s.apiUserToggle))
	mux.Handle("POST /admin/api/users/{id}/token", s.needAdmin(s.apiUserToken))
	mux.Handle("DELETE /admin/api/users/{id}", s.needAdmin(s.apiUserDelete))

	mux.Handle("GET /admin/api/channels", s.needAdmin(s.apiAdminChannels))
	mux.Handle("POST /admin/api/channels/{id}/toggle", s.needAdmin(s.apiChannelToggle))
	mux.Handle("POST /admin/api/channels/{id}/probe", s.needAdmin(s.apiChannelProbe))
	mux.Handle("POST /admin/api/channels/disable-failed", s.needAdmin(s.apiDisableFailed))

	mux.Handle("POST /admin/api/probe/start", s.needAdmin(s.apiProbeStart))
	mux.Handle("GET /admin/api/probe/status", s.needAdmin(s.apiProbeStatus))
	mux.Handle("POST /admin/api/probe/cancel", s.needAdmin(s.apiProbeCancel))

	mux.Handle("GET /admin/api/m3u", s.needAdmin(s.apiM3UGet))
	mux.Handle("POST /admin/api/m3u", s.needAdmin(s.apiM3USave))
	mux.Handle("POST /admin/api/m3u/fetch", s.needAdmin(s.apiM3UFetch))
	mux.Handle("POST /admin/api/m3u/upload", s.needAdmin(s.apiM3UUpload))
	mux.Handle("GET /admin/api/export.m3u", s.needAdmin(s.apiExportM3U))

	mux.Handle("POST /admin/api/settings", s.needAdmin(s.apiSettings))
	mux.Handle("POST /admin/api/sessions/{id}/stop", s.needAdmin(s.apiKillSession))

	mux.HandleFunc("GET /stream/{id}/{file...}", s.handleStreamFile)
	mux.HandleFunc("GET /proxy/{id}", s.handleProxy)
	mux.HandleFunc("GET /proxy/{id}/sub", s.handleProxySub)
	mux.HandleFunc("GET /s/{token}/playlist.m3u", s.handleExternalPlaylist)
	mux.HandleFunc("GET /s/{token}/channels.m3u", s.handleExternalPlaylist)

	return s.logRequests(mux)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") || strings.HasPrefix(r.URL.Path, "/stream/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		next.ServeHTTP(w, r)
		s.log.Debug("http", "method", r.Method, "path", r.URL.Path, "ip", auth.ClientIP(r), "ms", time.Since(start).Milliseconds())
	})
}

// ---------- 鉴权中间件 ----------

func (s *Server) needUser(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := s.currentUser(r)
		if u == nil {
			if wantsJSON(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录或登录已过期"})
				return
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxUser, u)))
	})
}

func (s *Server) needAdmin(h http.HandlerFunc) http.Handler {
	return s.needUser(func(w http.ResponseWriter, r *http.Request) {
		if u := userOf(r); u == nil || !u.IsAdmin() {
			if wantsJSON(r) {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "需要管理员权限"})
				return
			}
			http.Error(w, "需要管理员权限", http.StatusForbidden)
			return
		}
		h(w, r)
	})
}

func (s *Server) currentUser(r *http.Request) *store.User {
	if u := s.auth.UserFromRequest(r); u != nil {
		return u
	}
	if t := streamToken(r); t != "" {
		return s.auth.UserFromStreamToken(t)
	}
	return nil
}

// streamToken 取播放令牌：?token=... 或 X-Stream-Token 头。
func streamToken(r *http.Request) string {
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	return r.Header.Get("X-Stream-Token")
}

func userOf(r *http.Request) *store.User {
	u, _ := r.Context().Value(ctxUser).(*store.User)
	return u
}

func wantsJSON(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/admin/api/") {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// ---------- 渲染与 JSON 工具 ----------

type pageData struct {
	Title         string
	SiteTitle     string
	Version       string
	User          *store.User
	IsAdmin       bool
	BaseURL       string
	StreamURL     string
	Next          string
	Error         string
	Settings      []config.Field
	SettingValues map[string]string
	// 登录页人机验证
	TurnstileSiteKey string
	LoginWarn        string
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data pageData) {
	tpl, ok := s.tpl[name]
	if !ok {
		http.Error(w, "模板不存在: "+name, http.StatusInternalServerError)
		return
	}
	data.SiteTitle = s.Config().SiteTitle()
	data.Version = s.version
	if data.User == nil {
		data.User = userOf(r)
	}
	if data.User != nil {
		data.IsAdmin = data.User.IsAdmin()
		data.StreamURL = s.streamPlaylistURL(r, data.User)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := tpl.ExecuteTemplate(w, "layout.html", data); err != nil {
		s.log.Error("渲染模板失败", "page", name, "err", err)
	}
}

// streamPlaylistURL 生成供外部播放器（VLC/Kodi 等）使用的播放列表地址。
func (s *Server) streamPlaylistURL(r *http.Request, u *store.User) string {
	if u == nil || u.StreamToken == "" {
		return ""
	}
	return s.absoluteURL(r, "/s/"+u.StreamToken+"/playlist.m3u")
}

func (s *Server) absoluteURL(r *http.Request, path string) string {
	if s.baseURL != "" {
		return s.baseURL + path
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1"
	}
	return scheme + "://" + host + path
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	if err := dec.Decode(v); err != nil {
		return errors.New("请求内容解析失败: " + err.Error())
	}
	return nil
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("ID 非法")
	}
	return id, nil
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"humantime": humanTime,
		"yesno": func(b bool) string {
			if b {
				return "是"
			}
			return "否"
		},
	}
}

func humanTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04")
}
