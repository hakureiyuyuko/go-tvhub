// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

// Package store 封装 SQLite 存储：用户、会话、频道、收藏、设置。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，便于交叉编译
)

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("记录不存在")

// Store 是数据库句柄。
type Store struct {
	db   *sql.DB
	path string
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
  password_hash TEXT NOT NULL,
  role          TEXT NOT NULL DEFAULT 'user',
  enabled       INTEGER NOT NULL DEFAULT 1,
  stream_token  TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  last_login_at TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_token ON users(stream_token) WHERE stream_token <> '';

CREATE TABLE IF NOT EXISTS sessions (
  token      TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  ip         TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS channels (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT NOT NULL,
  url        TEXT NOT NULL UNIQUE,
  group_name TEXT NOT NULL DEFAULT '',
  logo       TEXT NOT NULL DEFAULT '',
  tvg_id     TEXT NOT NULL DEFAULT '',
  headers    TEXT NOT NULL DEFAULT '',
  sort_order INTEGER NOT NULL DEFAULT 0,
  disabled   INTEGER NOT NULL DEFAULT 0,
  disabled_auto INTEGER NOT NULL DEFAULT 0,
  probe      TEXT NOT NULL DEFAULT '',
  probe_at   TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_channels_order ON channels(sort_order);

CREATE TABLE IF NOT EXISTS favorites (
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL,
  PRIMARY KEY (user_id, channel_id)
);

CREATE TABLE IF NOT EXISTS settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);
`

// Open 打开（必要时创建）数据库并完成建表。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录失败: %w", err)
		}
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(8000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// SQLite 同一时刻只允许一个写者，单连接可以避免 SQLITE_BUSY，够用且最稳。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	s := &Store{db: db, path: path}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化表结构失败: %w", err)
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("升级表结构失败: %w", err)
	}
	return s, nil
}

// migrate 做轻量的增量升级：新表由 schema 建成，老库缺的列在这里补。
func (s *Store) migrate() error {
	// disabled_auto：区分「探测失败被面板自动停用」和「用户手动停用」。
	// 前者在探测通过 / 重新导入播放列表时自动恢复启用，后者不会被动。
	if ok, err := s.hasColumn("channels", "disabled_auto"); err != nil {
		return err
	} else if !ok {
		if _, err := s.db.Exec(`ALTER TABLE channels ADD COLUMN disabled_auto INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
		// 老数据分不清是谁停的，但探测失败会留下 “探测失败：…” 的结果，按自动停用回填
		if _, err := s.db.Exec(`UPDATE channels SET disabled_auto = 1 WHERE disabled = 1 AND probe LIKE '探测失败%'`); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) hasColumn(table, column string) (bool, error) {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// Path 返回数据库文件路径。
func (s *Store) Path() string { return s.path }

func nowTS() string { return time.Now().UTC().Format(time.RFC3339) }

func fmtTS(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func parseTS(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, v.String)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

func parseTSVal(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
