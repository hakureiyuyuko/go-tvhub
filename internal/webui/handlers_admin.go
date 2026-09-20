// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package webui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"tvhub/internal/auth"
	"tvhub/internal/config"
	"tvhub/internal/m3u"
	"tvhub/internal/stream"
)

type userItem struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	Role        string `json:"role"`
	Enabled     bool   `json:"enabled"`
	StreamToken string `json:"stream_token"`
	CreatedAt   string `json:"created_at"`
	LastLoginAt string `json:"last_login_at"`
}

// apiOverview 返回服务总览。
func (s *Server) apiOverview(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config()
	total, enabled, _ := s.st.CountChannels()
	users, _ := s.st.CountUsers()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	ver, ferr := s.ffmpegStatus()
	ffErr := ""
	if ferr != nil {
		ffErr = ferr.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":          s.version,
		"uptime":           humanDuration(time.Since(s.started)),
		"goroutines":       runtime.NumGoroutine(),
		"memory_mb":        float64(ms.Alloc) / 1024 / 1024,
		"users":            users,
		"channels_total":   total,
		"channels_enabled": enabled,
		"active_sessions":  s.mgr.Active(),
		"max_sessions":     cfg.Int(config.KeyMaxSessions, 8),
		"db_path":          s.st.Path(),
		"base_url":         s.baseURL,
		"sessions":         s.mgr.Stats(),
		"ffmpeg": map[string]any{
			"ok":      ferr == nil,
			"path":    cfg.Get(config.KeyFFmpegPath),
			"version": ver,
			"error":   ffErr,
		},
		"m3u": map[string]any{
			"source":     cfg.Get(config.KeyM3USource),
			"applied_at": cfg.Get(config.KeyM3UApplied),
		},
	})
}

// ffmpegStatus 检测 ffmpeg 可用性（结果缓存 30 秒）。
func (s *Server) ffmpegStatus() (string, error) {
	s.ffMu.Lock()
	defer s.ffMu.Unlock()
	if time.Since(s.ffAt) < 30*time.Second {
		return s.ffVer, s.ffErr
	}
	ver, err := s.Config().StreamOptions().FFmpegVersion()
	s.ffVer, s.ffErr, s.ffAt = ver, err, time.Now()
	return ver, err
}

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分 %d 秒", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d 小时 %d 分", int(d.Hours()), int(d.Minutes())%60)
}

// ---------- 用户管理 ----------

func (s *Server) apiUsers(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]userItem, 0, len(list))
	for _, u := range list {
		it := userItem{
			ID: u.ID, Username: u.Username, Role: u.Role, Enabled: u.Enabled,
			StreamToken: u.StreamToken, CreatedAt: u.CreatedAt.Local().Format("2006-01-02 15:04"),
		}
		if u.LastLoginAt != nil {
			it.LastLoginAt = u.LastLoginAt.Local().Format("2006-01-02 15:04")
		}
		if u.StreamToken != "" {
			it.StreamToken = u.StreamToken
		}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func validateUsername(name string) error {
	n := strings.TrimSpace(name)
	if len(n) < 2 || len(n) > 32 {
		return errors.New("用户名长度需为 2-32 个字符")
	}
	if strings.ContainsAny(n, " \t\r\n/\\\"'") {
		return errors.New("用户名不能包含空格或引号/斜杠")
	}
	return nil
}

func validatePassword(pw string) error {
	if len(pw) < 6 {
		return errors.New("密码至少 6 位")
	}
	if len(pw) > 128 {
		return errors.New("密码过长")
	}
	return nil
}

func (s *Server) apiUserCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if err := validateUsername(req.Username); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := validatePassword(req.Password); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Role != "admin" {
		req.Role = "user"
	}
	hash, err := s.auth.Hash(req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	id, err := s.st.CreateUser(req.Username, hash, req.Role)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeErr(w, http.StatusBadRequest, errors.New("用户名已存在"))
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	token := auth.NewToken(16)
	if err := s.st.SetUserStreamToken(id, token); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("新建用户", "user", req.Username, "role", req.Role, "by", userOf(r).Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "stream_token": token})
}

// ---------- 用户管理 ----------

func (s *Server) apiUserPassword(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := validatePassword(req.Password); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	hash, err := s.auth.Hash(req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.st.UpdateUserPassword(id, hash); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("重置密码", "target_id", id, "by", userOf(r).Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) apiUserRole(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Role != "admin" && req.Role != "user" {
		writeErr(w, http.StatusBadRequest, errors.New("角色非法"))
		return
	}
	me := userOf(r)
	if id == me.ID && req.Role != "admin" {
		writeErr(w, http.StatusBadRequest, errors.New("不能取消自己的管理员权限"))
		return
	}
	if req.Role != "admin" {
		target, err := s.st.UserByID(id)
		if err != nil {
			writeErr(w, http.StatusNotFound, errors.New("用户不存在"))
			return
		}
		if target.IsAdmin() {
			if n, _ := s.st.CountAdmins(); n <= 1 {
				writeErr(w, http.StatusBadRequest, errors.New("必须保留至少一个管理员"))
				return
			}
		}
	}
	if err := s.st.UpdateUserRole(id, req.Role); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) apiUserToggle(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	me := userOf(r)
	if id == me.ID && !req.Enabled {
		writeErr(w, http.StatusBadRequest, errors.New("不能停用自己"))
		return
	}
	target, err := s.st.UserByID(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("用户不存在"))
		return
	}
	if target.IsAdmin() && !req.Enabled {
		if n, _ := s.st.CountAdmins(); n <= 1 {
			writeErr(w, http.StatusBadRequest, errors.New("必须保留至少一个启用中的管理员"))
			return
		}
	}
	if err := s.st.SetUserEnabled(id, req.Enabled); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": req.Enabled})
}

func (s *Server) apiUserToken(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if _, err := s.st.UserByID(id); err != nil {
		writeErr(w, http.StatusNotFound, errors.New("用户不存在"))
		return
	}
	token := auth.NewToken(16)
	if err := s.st.SetUserStreamToken(id, token); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stream_token": token})
}

func (s *Server) apiUserDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	me := userOf(r)
	if id == me.ID {
		writeErr(w, http.StatusBadRequest, errors.New("不能删除自己"))
		return
	}
	target, err := s.st.UserByID(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("用户不存在"))
		return
	}
	if target.IsAdmin() {
		if n, _ := s.st.CountAdmins(); n <= 1 {
			writeErr(w, http.StatusBadRequest, errors.New("必须保留至少一个管理员"))
			return
		}
	}
	if err := s.st.DeleteUser(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("删除用户", "user", target.Username, "by", me.Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 频道管理 ----------

func (s *Server) apiAdminChannels(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListChannels(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	type item struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		Group     string `json:"group"`
		URL       string `json:"url"`
		Kind      string `json:"kind"`
		Disabled  bool   `json:"disabled"`
		Probe     string `json:"probe"`
		SortOrder int    `json:"sort_order"`
	}
	out := make([]item, 0, len(list))
	for _, c := range list {
		it := item{ID: c.ID, Name: c.Name, Group: c.Group, URL: c.URL, Kind: stream.KindLabel(c.URL), Disabled: c.Disabled, Probe: c.Probe, SortOrder: c.SortOrder}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": out})
}

func (s *Server) apiChannelToggle(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Disabled bool `json:"disabled"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if _, err := s.st.ChannelByID(id); err != nil {
		writeErr(w, http.StatusNotFound, errors.New("频道不存在"))
		return
	}
	if err := s.st.SetChannelDisabled(id, req.Disabled); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if req.Disabled {
		s.mgr.Stop(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "disabled": req.Disabled})
}

func (s *Server) apiChannelProbe(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ch, err := s.st.ChannelByID(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("频道不存在"))
		return
	}
	res := s.Config().StreamOptions().Probe(*ch)
	text := res.Summary
	if !res.OK {
		text = "探测失败：" + res.Error
	}
	if err := s.st.SetChannelProbe(id, text); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- 播放列表管理 ----------

func (s *Server) apiM3UGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config()
	writeJSON(w, http.StatusOK, map[string]any{
		"content":    cfg.Get(config.KeyM3UContent),
		"source":     cfg.Get(config.KeyM3USource),
		"applied_at": cfg.Get(config.KeyM3UApplied),
	})
}

// importM3U 解析并应用播放列表。
func (s *Server) importM3U(content, sourceName string) (res importSummary, err error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return res, errors.New("播放列表内容为空")
	}
	entries := m3u.ApplyGroups(m3u.Parse(content))
	if len(entries) == 0 {
		return res, errors.New("没有解析到任何频道，请检查文件格式（需要 #EXTM3U / #EXTINF）")
	}
	out, err := s.st.ImportChannels(entries)
	if err != nil {
		return res, err
	}
	settings := map[string]string{
		config.KeyM3UContent: content,
		config.KeyM3USource:  sourceName,
		config.KeyM3UApplied: time.Now().Format("2006-01-02 15:04:05"),
	}
	if err := s.st.SetSettings(settings); err != nil {
		return res, err
	}
	s.InvalidateConfig()
	res = importSummary{
		Added: out.Added, Updated: out.Updated, Removed: out.Removed,
		Total: out.Total, Kept: out.Kept, Source: sourceName,
	}
	s.log.Info("导入播放列表", "source", sourceName, "added", out.Added, "updated", out.Updated, "removed", out.Removed)
	return res, nil
}

type importSummary struct {
	Added   int    `json:"added"`
	Updated int    `json:"updated"`
	Removed int    `json:"removed"`
	Kept    int    `json:"kept"`
	Total   int    `json:"total"`
	Source  string `json:"source"`
}

func (s *Server) apiM3USave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Content string `json:"content"`
		Source  string `json:"source"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.importM3U(req.Content, firstNonEmpty(req.Source, "手工粘贴"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}

func (s *Server) apiM3UFetch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		writeErr(w, http.StatusBadRequest, errors.New("请填写 http(s) 开头的地址"))
		return
	}
	body, err := s.downloadPlaylist(r, req.URL)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	res, err := s.importM3U(body, req.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}

func (s *Server) apiM3UUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("上传内容解析失败: "+err.Error()))
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("没有收到文件"))
		return
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, 16<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("读取上传文件失败"))
		return
	}
	res, err := s.importM3U(string(b), "上传文件: "+hdr.Filename)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}

func (s *Server) downloadPlaylist(r *http.Request, url string) (string, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "TvHub/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", errors.New("下载失败: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载失败，源站返回 %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", errors.New("读取内容失败: " + err.Error())
	}
	return strings.TrimPrefix(string(b), "\ufeff"), nil
}

func (s *Server) apiExportM3U(w http.ResponseWriter, r *http.Request) {
	entries, err := s.st.ExportEntries(false)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="channels.m3u"`)
	_, _ = io.WriteString(w, m3u.Render(entries))
}

// ---------- 设置与会话 ----------

func (s *Server) apiSettings(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	allowed := map[string]bool{}
	for _, f := range config.Fields() {
		allowed[f.Key] = true
	}
	save := map[string]string{}
	for k, v := range req {
		if allowed[k] {
			save[k] = strings.TrimSpace(v)
		}
	}
	if len(save) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("没有可保存的设置项"))
		return
	}
	if err := s.st.SetSettings(save); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.InvalidateConfig()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved": len(save)})
}

// ---------- 批量探测 ----------

// apiProbeStart 启动「一键探测」后台任务。
func (s *Server) apiProbeStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AutoDisable     bool `json:"auto_disable"`
		IncludeDisabled bool `json:"include_disabled"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	st, err := s.startProbe(req.AutoDisable, req.IncludeDisabled)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "status": st})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// apiProbeStatus 查询探测进度。
func (s *Server) apiProbeStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.probe.Status())
}

// apiProbeCancel 停止后续探测。
func (s *Server) apiProbeCancel(w http.ResponseWriter, r *http.Request) {
	s.probe.Cancel()
	writeJSON(w, http.StatusOK, s.probe.Status())
}

// apiDisableFailed 把所有已标记为探测失败的频道批量停用。
func (s *Server) apiDisableFailed(w http.ResponseWriter, r *http.Request) {
	n := s.disableFailedChannels(r.Context())
	if n > 0 {
		s.log.Info("批量停用探测失败的频道", "count", n, "by", userOf(r).Username)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "disabled": n})
}

func (s *Server) apiKillSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stopped": s.mgr.Stop(id)})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
