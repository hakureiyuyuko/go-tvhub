// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package store

import (
	"database/sql"
	"time"

	"tvhub/internal/m3u"
)

// Channel 是一条直播频道。
type Channel struct {
	ID        int64
	Name      string
	URL       string
	Group     string
	Logo      string
	TvgID     string
	Headers   string
	SortOrder int
	Disabled  bool
	Probe     string
	ProbeAt   *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Kind 返回该频道源地址的协议类型。
func (c *Channel) Kind() string { return m3u.Kind(c.URL) }

const channelCols = `id, name, url, group_name, logo, tvg_id, headers, sort_order, disabled, probe, probe_at, created_at, updated_at`

func scanChannel(sc interface{ Scan(...any) error }) (*Channel, error) {
	var (
		c        Channel
		disabled int
		probeAt  sql.NullString
		created  string
		updated  string
	)
	if err := sc.Scan(&c.ID, &c.Name, &c.URL, &c.Group, &c.Logo, &c.TvgID, &c.Headers,
		&c.SortOrder, &disabled, &c.Probe, &probeAt, &created, &updated); err != nil {
		return nil, err
	}
	c.Disabled = disabled != 0
	c.ProbeAt = parseTS(probeAt)
	c.CreatedAt = parseTSVal(created)
	c.UpdatedAt = parseTSVal(updated)
	return &c, nil
}

// ImportResult 描述一次播放列表导入的结果。
type ImportResult struct {
	Added   int `json:"added"`
	Updated int `json:"updated"`
	Removed int `json:"removed"`
	Total   int `json:"total"`
	Kept    int `json:"kept"`
}

// ImportChannels 用新的播放列表覆盖频道表：按地址 upsert，播放列表里已消失的频道删除。
// 频道的启用状态、探测结果和用户收藏会被保留。
func (s *Store) ImportChannels(entries []m3u.Entry) (ImportResult, error) {
	var res ImportResult
	tx, err := s.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	type old struct {
		id                                int64
		name, group, logo, tvgID, headers string
		sortOrder                         int
	}
	existing := map[string]old{}
	rows, err := tx.Query(`SELECT id, name, url, group_name, logo, tvg_id, headers, sort_order FROM channels`)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var o old
		var url string
		if err := rows.Scan(&o.id, &o.name, &url, &o.group, &o.logo, &o.tvgID, &o.headers, &o.sortOrder); err != nil {
			rows.Close()
			return res, err
		}
		existing[url] = o
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	now := nowTS()
	seen := make(map[string]bool, len(entries))
	for i, e := range entries {
		seen[e.URL] = true
		if o, ok := existing[e.URL]; ok {
			if o.name != e.Name || o.group != e.Group || o.logo != e.Logo || o.tvgID != e.TvgID ||
				o.headers != e.Headers || o.sortOrder != i {
				if _, err := tx.Exec(
					`UPDATE channels SET name=?, group_name=?, logo=?, tvg_id=?, headers=?, sort_order=?, updated_at=? WHERE id=?`,
					e.Name, e.Group, e.Logo, e.TvgID, e.Headers, i, now, o.id); err != nil {
					return res, err
				}
				res.Updated++
			} else {
				res.Kept++
			}
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO channels (name, url, group_name, logo, tvg_id, headers, sort_order, disabled, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
			e.Name, e.URL, e.Group, e.Logo, e.TvgID, e.Headers, i, now, now); err != nil {
			return res, err
		}
		res.Added++
	}
	for url, o := range existing {
		if seen[url] {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM channels WHERE id = ?`, o.id); err != nil {
			return res, err
		}
		res.Removed++
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	res.Total = len(entries)
	return res, nil
}

// ListChannels 返回频道列表，includeDisabled 为 false 时只返回启用中的频道。
func (s *Store) ListChannels(includeDisabled bool) ([]Channel, error) {
	q := `SELECT ` + channelCols + ` FROM channels`
	if !includeDisabled {
		q += ` WHERE disabled = 0`
	}
	q += ` ORDER BY sort_order, id`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ChannelByID 按 ID 取频道。
func (s *Store) ChannelByID(id int64) (*Channel, error) {
	c, err := scanChannel(s.db.QueryRow(`SELECT `+channelCols+` FROM channels WHERE id = ?`, id))
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return c, nil
}

// SetChannelDisabled 启用/停用频道。
func (s *Store) SetChannelDisabled(id int64, disabled bool) error {
	_, err := s.db.Exec(`UPDATE channels SET disabled = ?, updated_at = ? WHERE id = ?`, boolInt(disabled), nowTS(), id)
	return err
}

// SetChannelProbe 保存探测结果。
func (s *Store) SetChannelProbe(id int64, probe string) error {
	_, err := s.db.Exec(`UPDATE channels SET probe = ?, probe_at = ? WHERE id = ?`, probe, nowTS(), id)
	return err
}

// CountChannels 返回频道总数与启用数。
func (s *Store) CountChannels() (total, enabled int, err error) {
	err = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(disabled = 0), 0) FROM channels`).Scan(&total, &enabled)
	return
}

// ExportEntries 把频道表导出成 m3u 条目。
func (s *Store) ExportEntries(includeDisabled bool) ([]m3u.Entry, error) {
	list, err := s.ListChannels(includeDisabled)
	if err != nil {
		return nil, err
	}
	out := make([]m3u.Entry, 0, len(list))
	for _, c := range list {
		out = append(out, m3u.Entry{
			Name:    c.Name,
			URL:     c.URL,
			Group:   c.Group,
			Logo:    c.Logo,
			TvgID:   c.TvgID,
			Headers: c.Headers,
		})
	}
	return out, nil
}

// ToggleFavorite 切换收藏状态，返回切换后的状态。
func (s *Store) ToggleFavorite(userID, channelID int64) (bool, error) {
	var exists int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM favorites WHERE user_id = ? AND channel_id = ?`, userID, channelID).Scan(&exists)
	if err != nil {
		return false, err
	}
	if exists > 0 {
		_, err := s.db.Exec(`DELETE FROM favorites WHERE user_id = ? AND channel_id = ?`, userID, channelID)
		return false, err
	}
	_, err = s.db.Exec(`INSERT INTO favorites (user_id, channel_id, created_at) VALUES (?, ?, ?)`, userID, channelID, nowTS())
	return true, err
}

// FavoriteIDs 返回该用户收藏的频道 ID 集合。
func (s *Store) FavoriteIDs(userID int64) (map[int64]bool, error) {
	rows, err := s.db.Query(`SELECT channel_id FROM favorites WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
