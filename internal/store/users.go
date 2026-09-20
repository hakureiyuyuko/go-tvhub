// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package store

import (
	"database/sql"
	"errors"
	"time"
)

// User 是面板用户。
type User struct {
	ID          int64
	Username    string
	Role        string // admin | user
	Enabled     bool
	StreamToken string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	LastLoginAt *time.Time
}

// IsAdmin 判断是否管理员。
func (u *User) IsAdmin() bool { return u.Role == "admin" }

const userCols = `id, username, role, enabled, stream_token, created_at, updated_at, last_login_at`

func scanUser(sc interface{ Scan(...any) error }) (*User, error) {
	var (
		u       User
		enabled int
		created string
		updated string
		last    sql.NullString
	)
	if err := sc.Scan(&u.ID, &u.Username, &u.Role, &enabled, &u.StreamToken, &created, &updated, &last); err != nil {
		return nil, err
	}
	u.Enabled = enabled != 0
	u.CreatedAt = parseTSVal(created)
	u.UpdatedAt = parseTSVal(updated)
	u.LastLoginAt = parseTS(last)
	return &u, nil
}

// CreateUser 新建用户，返回自增 ID。
func (s *Store) CreateUser(username, passwordHash, role string) (int64, error) {
	now := nowTS()
	res, err := s.db.Exec(
		`INSERT INTO users (username, password_hash, role, enabled, stream_token, created_at, updated_at)
		 VALUES (?, ?, ?, 1, '', ?, ?)`, username, passwordHash, role, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UserByID 按 ID 查询用户。
func (s *Store) UserByID(id int64) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// UserByUsername 按用户名查询（大小写不敏感）。
func (s *Store) UserByUsername(name string) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE username = ?`, name))
}

// UserByStreamToken 按播放令牌查询用户。
func (s *Store) UserByStreamToken(token string) (*User, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE stream_token = ? AND stream_token <> ''`, token))
}

// PasswordHash 读取密码哈希。
func (s *Store) PasswordHash(userID int64) (string, error) {
	var h string
	err := s.db.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return h, err
}

// ListUsers 返回全部用户（按创建时间倒序）。
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// CountUsers 返回用户总数。
func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CountAdmins 返回启用状态的管理员数量。
func (s *Store) CountAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin' AND enabled = 1`).Scan(&n)
	return n, err
}

// UpdateUserPassword 更新密码哈希，同时踢掉该用户的所有会话。
func (s *Store) UpdateUserPassword(id int64, passwordHash string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, passwordHash, nowTS(), id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateUserRole 修改角色。
func (s *Store) UpdateUserRole(id int64, role string) error {
	_, err := s.db.Exec(`UPDATE users SET role = ?, updated_at = ? WHERE id = ?`, role, nowTS(), id)
	return err
}

// SetUserEnabled 启用/停用用户，停用时踢掉其会话。
func (s *Store) SetUserEnabled(id int64, enabled bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE users SET enabled = ?, updated_at = ? WHERE id = ?`, boolInt(enabled), nowTS(), id); err != nil {
		return err
	}
	if !enabled {
		if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetUserStreamToken 设置播放令牌。
func (s *Store) SetUserStreamToken(id int64, token string) error {
	_, err := s.db.Exec(`UPDATE users SET stream_token = ?, updated_at = ? WHERE id = ?`, token, nowTS(), id)
	return err
}

// DeleteUser 删除用户。
func (s *Store) DeleteUser(id int64) error {
	_, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}

// TouchUserLogin 记录登录时间。
func (s *Store) TouchUserLogin(id int64) error {
	_, err := s.db.Exec(`UPDATE users SET last_login_at = ? WHERE id = ?`, nowTS(), id)
	return err
}

// CreateSession 写入一条登录会话。
func (s *Store) CreateSession(token string, userID int64, expires time.Time, ip, ua string) error {
	_, err := s.db.Exec(
		`INSERT INTO sessions (token, user_id, created_at, expires_at, ip, user_agent) VALUES (?, ?, ?, ?, ?, ?)`,
		token, userID, nowTS(), fmtTS(expires), ip, ua)
	return err
}

// SessionUser 根据会话令牌取用户（已过期/被停用则返回 ErrNotFound）。
func (s *Store) SessionUser(token string) (*User, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	u, err := scanUser(s.db.QueryRow(
		`SELECT `+userCols+` FROM users
		 WHERE id = (SELECT user_id FROM sessions WHERE token = ? AND expires_at > ?)`, token, nowTS()))
	if err != nil {
		return nil, ErrNotFound
	}
	if !u.Enabled {
		return nil, ErrNotFound
	}
	return u, nil
}

// DeleteSession 删除会话。
func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// DeleteUserSessions 踢掉某用户全部会话。
func (s *Store) DeleteUserSessions(userID int64) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// PurgeExpiredSessions 清理过期会话。
func (s *Store) PurgeExpiredSessions() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, nowTS())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
