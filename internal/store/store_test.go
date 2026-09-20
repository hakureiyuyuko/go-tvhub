// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package store

import (
	"path/filepath"
	"testing"
	"time"

	"tvhub/internal/m3u"
)

func timeNowPlus(sec int) time.Time { return time.Now().Add(time.Duration(sec) * time.Second) }

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sub", "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUserLifecycle(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateUser("alice", "hash1", "admin")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateUser("alice", "hash2", "user"); err == nil {
		t.Fatal("expected duplicate username error")
	}
	u, err := s.UserByUsername("ALICE") // COLLATE NOCASE
	if err != nil {
		t.Fatalf("by name: %v", err)
	}
	if u.ID != id || !u.IsAdmin() || !u.Enabled {
		t.Fatalf("unexpected user %+v", u)
	}
	if err := s.SetUserStreamToken(id, "tok123"); err != nil {
		t.Fatal(err)
	}
	u2, err := s.UserByStreamToken("tok123")
	if err != nil || u2.ID != id {
		t.Fatalf("by token: %v %+v", err, u2)
	}
	// 会话
	if err := s.CreateSession("sess1", id, timeNowPlus(3600), "127.0.0.1", "test"); err != nil {
		t.Fatal(err)
	}
	if su, err := s.SessionUser("sess1"); err != nil || su.ID != id {
		t.Fatalf("session user: %v %+v", err, su)
	}
	if err := s.UpdateUserPassword(id, "hash3"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser("sess1"); err == nil {
		t.Fatal("session should be revoked after password change")
	}
	// 过期会话
	if err := s.CreateSession("sess2", id, timeNowPlus(-10), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser("sess2"); err == nil {
		t.Fatal("expired session must not authenticate")
	}
	n, err := s.PurgeExpiredSessions()
	if err != nil || n != 1 {
		t.Fatalf("purge: %v n=%d", err, n)
	}
	if err := s.SetUserEnabled(id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser("sess1"); err == nil {
		t.Fatal("disabled user must not authenticate")
	}
	if err := s.DeleteUser(id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserByID(id); err == nil {
		t.Fatal("user should be gone")
	}
}

func TestImportChannels(t *testing.T) {
	s := newTestStore(t)
	uid, err := s.CreateUser("bob", "h", "user")
	if err != nil {
		t.Fatal(err)
	}
	entries := m3u.ApplyGroups(m3u.Parse("#EXTM3U\n#EXTINF:-1,CCTV1\nrtsp://a/1\n#EXTINF:-1,湖南卫视\nrtsp://a/2\n"))
	res, err := s.ImportChannels(entries)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 2 || res.Total != 2 {
		t.Fatalf("res=%+v", res)
	}
	list, _ := s.ListChannels(false)
	if len(list) != 2 || list[0].Group != "央视" || list[1].Group != "卫视" {
		t.Fatalf("list=%+v", list)
	}
	// 收藏应保留
	if _, err := s.ToggleFavorite(uid, list[0].ID); err != nil {
		t.Fatal(err)
	}
	// 改名 + 删除一条
	res2, err := s.ImportChannels([]m3u.Entry{{Name: "CCTV-1 综合", URL: "rtsp://a/1", Group: "央视"}})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Updated != 1 || res2.Removed != 1 || res2.Total != 1 {
		t.Fatalf("res2=%+v", res2)
	}
	l2, _ := s.ListChannels(true)
	if len(l2) != 1 || l2[0].Name != "CCTV-1 综合" {
		t.Fatalf("l2=%+v", l2)
	}
	if _, err := s.ChannelByID(999999); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := s.SetChannelDisabled(l2[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if on, _ := s.ListChannels(false); len(on) != 0 {
		t.Fatalf("disabled channel should be hidden: %+v", on)
	}
	total, enabled, err := s.CountChannels()
	if err != nil || total != 1 || enabled != 0 {
		t.Fatalf("count total=%d enabled=%d err=%v", total, enabled, err)
	}
	favs, _ := s.FavoriteIDs(uid)
	if !favs[list[0].ID] {
		t.Fatalf("favorite lost: %+v", favs)
	}
}

func TestSettings(t *testing.T) {
	s := newTestStore(t)
	if got := s.Setting("none", "def"); got != "def" {
		t.Fatalf("got %q", got)
	}
	if err := s.SetSettings(map[string]string{"a": "1", "b": "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("a", "2"); err != nil {
		t.Fatal(err)
	}
	if got := s.SettingInt("a", 0); got != 2 {
		t.Fatalf("got %d", got)
	}
	if got := s.SettingInt("zzz", 7); got != 7 {
		t.Fatalf("got %d", got)
	}
	if err := s.SetSetting("flag", "on"); err != nil {
		t.Fatal(err)
	}
	if !s.SettingBool("flag", false) {
		t.Fatal("want true")
	}
	all, err := s.AllSettings()
	if err != nil || len(all) != 3 {
		t.Fatalf("all=%+v err=%v", all, err)
	}
}
