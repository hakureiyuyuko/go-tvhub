// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package webui

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"tvhub/internal/stream"
)

type channelItem struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Group    string `json:"group"`
	Logo     string `json:"logo"`
	Kind     string `json:"kind"`
	Favorite bool   `json:"favorite"`
}

// apiChannels 返回当前用户可见的频道列表。
func (s *Server) apiChannels(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	list, err := s.st.ListChannels(false)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	favs, _ := s.st.FavoriteIDs(u.ID)
	out := make([]channelItem, 0, len(list))
	for _, c := range list {
		out = append(out, channelItem{
			ID: c.ID, Name: c.Name, Group: c.Group, Logo: c.Logo,
			Kind: stream.KindLabel(c.URL), Favorite: favs[c.ID],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": out, "count": len(out)})
}

// apiFavorite 切换收藏。
func (s *Server) apiFavorite(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if _, err := s.st.ChannelByID(id); err != nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("频道不存在"))
		return
	}
	fav, err := s.st.ToggleFavorite(u.ID, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"favorite": fav})
}

// apiPrepare 启动转发（或直接给出代理地址），返回可播放地址。
func (s *Server) apiPrepare(w http.ResponseWriter, r *http.Request) {
	ch, err := s.channelFromPath(r)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	resp := map[string]any{
		"state":      "ready",
		"channel_id": ch.ID,
		"name":       ch.Name,
		"kind":       stream.KindLabel(ch.URL),
	}
	if ch.Kind() == "http" {
		resp["method"] = "proxy"
		resp["playlist"] = fmt.Sprintf("/proxy/%d", ch.ID)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	sess, err := s.mgr.Ready(ctx, *ch, 30*time.Second)
	if err != nil {
		s.log.Warn("启动转发失败", "channel", ch.ID, "name", ch.Name, "err", err)
		resp["state"] = "error"
		resp["error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["method"] = "hls"
	resp["playlist"] = fmt.Sprintf("/stream/%d/index.m3u8", ch.ID)
	resp["viewers"] = sess.Viewers()
	writeJSON(w, http.StatusOK, resp)
}

// apiStatus 查询转发会话状态。
func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sess := s.mgr.Get(id)
	if sess == nil {
		writeJSON(w, http.StatusOK, map[string]any{"state": "idle", "channel_id": id})
		return
	}
	writeJSON(w, http.StatusOK, sess.Stat())
}

// apiStopStream 停止某频道的转发。
func (s *Server) apiStopStream(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stopped": s.mgr.Stop(id)})
}
