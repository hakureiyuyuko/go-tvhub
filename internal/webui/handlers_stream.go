// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package webui

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"

	"tvhub/internal/auth"
	"tvhub/internal/store"
)

// streamPath 返回某频道在浏览器端应使用的播放地址。
func streamPath(ch store.Channel) string {
	if ch.Kind() == "http" {
		return fmt.Sprintf("/proxy/%d", ch.ID)
	}
	return fmt.Sprintf("/stream/%d/index.m3u8", ch.ID)
}

// channelFromPath 取出路径里的频道 ID 并校验。
func (s *Server) channelFromPath(r *http.Request) (*store.Channel, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("频道 ID 非法")
	}
	ch, err := s.st.ChannelByID(id)
	if err != nil {
		return nil, fmt.Errorf("频道不存在")
	}
	if ch.Disabled {
		return nil, fmt.Errorf("该频道已被停用")
	}
	return ch, nil
}

// extraQuery 在外部播放器用令牌访问时，把令牌透传到子请求上。
func extraQuery(r *http.Request) string {
	if t := streamToken(r); t != "" {
		return "token=" + t
	}
	return ""
}

// handleStreamFile 提供 HLS 播放列表与分片。
func (s *Server) handleStreamFile(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if u == nil {
		http.Error(w, "未授权：请重新登录", http.StatusUnauthorized)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "频道 ID 非法", http.StatusBadRequest)
		return
	}
	name := path.Base(r.PathValue("file"))
	sess := s.mgr.Get(id)
	if sess == nil {
		http.Error(w, "直播流已断开，请重新点播", http.StatusNotFound)
		return
	}
	sess.TouchClient(clientKey(r, u.Username))
	if strings.HasSuffix(strings.ToLower(name), ".m3u8") {
		b, err := sess.Playlist()
		if err != nil || len(b) == 0 {
			http.Error(w, "播放列表尚未生成", http.StatusNotFound)
			return
		}
		token := streamToken(r)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-cache, no-store")
		if token == "" {
			_, _ = w.Write(b)
			return
		}
		// 外部播放器解析相对地址时会丢掉查询串，这里给分片地址补上令牌
		for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			t := strings.TrimSpace(line)
			if t != "" && !strings.HasPrefix(t, "#") {
				t += "?token=" + token
			}
			_, _ = io.WriteString(w, t+"\n")
		}
		return
	}

	p, err := sess.SegmentPath(name)
	if err != nil {
		http.Error(w, "分片不存在", http.StatusNotFound)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, "分片读取失败", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "分片读取失败", http.StatusNotFound)
		return
	}
	if strings.HasSuffix(strings.ToLower(name), ".ts") {
		w.Header().Set("Content-Type", "video/mp2t")
	} else if strings.HasSuffix(strings.ToLower(name), ".m4s") {
		w.Header().Set("Content-Type", "video/iso.segment")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// handleProxy 转发 http(s) 源。
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	if s.currentUser(r) == nil {
		http.Error(w, "未授权：请重新登录", http.StatusUnauthorized)
		return
	}
	ch, err := s.channelFromPath(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if ch.Kind() != "http" {
		http.Error(w, "该频道不是 HTTP 源", http.StatusBadRequest)
		return
	}
	s.proxy.Serve(w, r, *ch, extraQuery(r))
}

// handleProxySub 转发播放列表里引用的分片。
func (s *Server) handleProxySub(w http.ResponseWriter, r *http.Request) {
	if s.currentUser(r) == nil {
		http.Error(w, "未授权：请重新登录", http.StatusUnauthorized)
		return
	}
	ch, err := s.channelFromPath(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.proxy.ServeSub(w, r, *ch, extraQuery(r))
}

// handleExternalPlaylist 输出可供 VLC / Kodi / 电视盒子直接使用的播放列表。
func (s *Server) handleExternalPlaylist(w http.ResponseWriter, r *http.Request) {
	u := s.auth.UserFromStreamToken(r.PathValue("token"))
	if u == nil {
		http.Error(w, "播放令牌无效或已被重置", http.StatusForbidden)
		return
	}
	list, err := s.st.ListChannels(false)
	if err != nil {
		http.Error(w, "读取频道失败", http.StatusInternalServerError)
		return
	}
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	for _, ch := range list {
		url := s.absoluteURL(r, streamPath(ch)) + "?token=" + u.StreamToken
		sb.WriteString("#EXTINF:-1")
		if ch.TvgID != "" {
			fmt.Fprintf(&sb, ` tvg-id="%s"`, ch.TvgID)
		}
		if ch.Logo != "" {
			fmt.Fprintf(&sb, ` tvg-logo="%s"`, ch.Logo)
		}
		if ch.Group != "" {
			fmt.Fprintf(&sb, ` group-title="%s"`, ch.Group)
		}
		fmt.Fprintf(&sb, ",%s\n%s\n", ch.Name, url)
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="tvhub.m3u"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, sb.String())
}

func clientKey(r *http.Request, user string) string {
	if c, err := r.Cookie(auth.CookieName); err == nil && c.Value != "" {
		n := len(c.Value)
		if n > 12 {
			n = 12
		}
		return user + ":" + c.Value[:n]
	}
	return user + ":" + r.RemoteAddr
}
