// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"tvhub/internal/m3u"
)

// 因探测失败被面板自动停用的频道，重新导入播放列表时应自动恢复启用。
func TestImportReenablesAutoDisabled(t *testing.T) {
	s := newTestStore(t)
	entries := m3u.Parse("#EXTM3U\n#EXTINF:-1,CCTV1\nrtsp://a/1\n")
	if _, err := s.ImportChannels(entries, true); err != nil {
		t.Fatal(err)
	}
	ch, _ := s.ChannelByID(1)
	if err := s.SetChannelAutoDisabled(ch.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ChannelByID(ch.ID)
	if !got.Disabled || !got.DisabledAuto {
		t.Fatalf("应处于自动停用状态: %+v", got)
	}

	res, err := s.ImportChannels(entries, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reenabled != 1 {
		t.Fatalf("Reenabled=%d，期望 1（res=%+v）", res.Reenabled, res)
	}
	got, _ = s.ChannelByID(ch.ID)
	if got.Disabled || got.DisabledAuto {
		t.Fatalf("导入后应恢复启用: %+v", got)
	}
	if n, _ := s.ListChannels(false); len(n) != 1 {
		t.Fatalf("恢复后应出现在启用列表里: %+v", n)
	}
}

// 用户手动停用的频道，重新导入播放列表时不能被动恢复。
func TestImportKeepsManualDisabled(t *testing.T) {
	s := newTestStore(t)
	entries := m3u.Parse("#EXTM3U\n#EXTINF:-1,CCTV1\nrtsp://a/1\n")
	if _, err := s.ImportChannels(entries, true); err != nil {
		t.Fatal(err)
	}
	ch, _ := s.ChannelByID(1)
	if err := s.SetChannelDisabled(ch.ID, true); err != nil { // 手动停用
		t.Fatal(err)
	}
	res, err := s.ImportChannels(entries, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reenabled != 0 {
		t.Fatalf("手动停用的不该被恢复，Reenabled=%d", res.Reenabled)
	}
	got, _ := s.ChannelByID(ch.ID)
	if !got.Disabled || got.DisabledAuto {
		t.Fatalf("应保持手动停用: %+v", got)
	}
	// 手动重新启用后标记应清干净
	if err := s.SetChannelEnabled(ch.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ChannelByID(ch.ID)
	if got.Disabled || got.DisabledAuto {
		t.Fatalf("启用后状态应清干净: %+v", got)
	}
}

// 启动时的 -import（reenableAuto=false）不能动停用状态，
// 否则每次重启都会把已停用的频道又放出来。
func TestImportWithoutReenable(t *testing.T) {
	s := newTestStore(t)
	entries := m3u.Parse("#EXTM3U\n#EXTINF:-1,CCTV1\nrtsp://a/1\n")
	if _, err := s.ImportChannels(entries, false); err != nil {
		t.Fatal(err)
	}
	ch, _ := s.ChannelByID(1)
	if err := s.SetChannelAutoDisabled(ch.ID); err != nil {
		t.Fatal(err)
	}
	res, err := s.ImportChannels(entries, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reenabled != 0 {
		t.Fatalf("启动导入不应恢复启用，Reenabled=%d", res.Reenabled)
	}
	got, _ := s.ChannelByID(ch.ID)
	if !got.Disabled {
		t.Fatalf("启动导入后应仍处于停用: %+v", got)
	}
}

// EnableAllDisabled 一键恢复全部停用频道。
func TestEnableAllDisabled(t *testing.T) {
	s := newTestStore(t)
	entries := m3u.Parse("#EXTM3U\n#EXTINF:-1,A\nrtsp://a/1\n#EXTINF:-1,B\nrtsp://a/2\n")
	if _, err := s.ImportChannels(entries, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChannelDisabled(1, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChannelAutoDisabled(2); err != nil {
		t.Fatal(err)
	}
	n, err := s.EnableAllDisabled()
	if err != nil || n != 2 {
		t.Fatalf("enabled=%d err=%v", n, err)
	}
	if list, _ := s.ListChannels(false); len(list) != 2 {
		t.Fatalf("恢复后应都可见: %+v", list)
	}
}

// 老库没有 disabled_auto 列：打开时补列，并按探测结果回填历史数据。
func TestMigrateBackfillsDisabledAuto(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE channels (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		url TEXT NOT NULL UNIQUE,
		group_name TEXT NOT NULL DEFAULT '',
		logo TEXT NOT NULL DEFAULT '',
		tvg_id TEXT NOT NULL DEFAULT '',
		headers TEXT NOT NULL DEFAULT '',
		sort_order INTEGER NOT NULL DEFAULT 0,
		disabled INTEGER NOT NULL DEFAULT 0,
		probe TEXT NOT NULL DEFAULT '',
		probe_at TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name, url, probe string
		disabled         int
	}{
		{"自动停的", "rtsp://a/1", "探测失败：超时", 1},
		{"手动停的", "rtsp://a/2", "", 1},
		{"正常在用", "rtsp://a/3", "h264 1920x1080", 0},
	} {
		if _, err := db.Exec(
			`INSERT INTO channels (name, url, disabled, probe, created_at, updated_at) VALUES (?, ?, ?, ?, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`,
			row.name, row.url, row.disabled, row.probe); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err := Open(path) // 这里会跑迁移
	if err != nil {
		t.Fatalf("open old db: %v", err)
	}
	defer s.Close()
	list, err := s.ListChannels(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("频道数不对: %+v", list)
	}
	want := map[string]bool{"自动停的": true, "手动停的": false, "正常在用": false}
	for _, c := range list {
		if c.DisabledAuto != want[c.Name] {
			t.Errorf("%s 的 DisabledAuto=%v，期望 %v（probe=%q）", c.Name, c.DisabledAuto, want[c.Name], c.Probe)
		}
	}
}
