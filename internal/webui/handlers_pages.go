// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package webui

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"tvhub/internal/auth"
	"tvhub/internal/config"
)

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if u := s.auth.UserFromRequest(r); u != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	cfg := s.Config()
	data := pageData{
		Title: "登录",
		Next:  safeNext(r.URL.Query().Get("next")),
		Error: r.URL.Query().Get("err"),
	}
	switch {
	case cfg.TurnstileActive():
		data.TurnstileSiteKey = cfg.TurnstileSiteKey()
	case cfg.TurnstileEnabled():
		data.LoginWarn = "管理员已开启人机验证，但站点密钥/密钥未填写，当前不会强制校验"
	}
	s.render(w, r, "login", data)
}

func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/login?err="+url.QueryEscape("表单解析失败"), http.StatusFound)
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))

	// 登录页人机验证（启用且配齐密钥时才强制）
	cfg := s.Config()
	if cfg.TurnstileActive() {
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		res, err := auth.VerifyTurnstile(ctx, cfg.TurnstileSecret(), r.PostFormValue("cf-turnstile-response"), auth.ClientIP(r))
		cancel()
		switch {
		case err != nil:
			// 校验服务不可达（例如纯内网）：放行并告警，避免把自己锁在外面
			s.log.Warn("人机验证服务不可用，本次登录放行", "err", err, "ip", auth.ClientIP(r))
		case res == nil || !res.Success:
			codes := []string{}
			if res != nil {
				codes = res.ErrorCodes
			}
			msg := auth.TurnstileErrorText(codes)
			s.log.Warn("人机验证未通过", "ip", auth.ClientIP(r), "codes", codes)
			http.Redirect(w, r, "/login?err="+url.QueryEscape(msg)+"&next="+url.QueryEscape(next), http.StatusFound)
			return
		}
	}

	u, err := s.auth.Authenticate(username, password, auth.ClientIP(r))
	if err != nil {
		s.log.Warn("登录失败", "user", username, "ip", auth.ClientIP(r), "err", err)
		http.Redirect(w, r, "/login?err="+url.QueryEscape(err.Error())+"&next="+url.QueryEscape(next), http.StatusFound)
		return
	}
	if err := s.auth.StartSession(w, r, u); err != nil {
		s.log.Error("创建会话失败", "err", err)
		http.Redirect(w, r, "/login?err="+url.QueryEscape("创建会话失败: "+err.Error()), http.StatusFound)
		return
	}
	_ = s.st.TouchUserLogin(u.ID)
	s.log.Info("登录成功", "user", u.Username, "ip", auth.ClientIP(r))
	http.Redirect(w, r, next, http.StatusFound)
}

// safeNext 只允许跳回站内地址。
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.EndSession(w, r)
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) handlePlayer(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "player", pageData{Title: "直播"})
}

func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config()
	fields := config.Fields()
	vals := make(map[string]string, len(fields))
	for _, f := range fields {
		vals[f.Key] = cfg.Get(f.Key)
	}
	s.render(w, r, "admin", pageData{
		Title:         "管理",
		Settings:      fields,
		SettingValues: vals,
	})
}
