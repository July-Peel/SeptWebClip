// Package store 用 SQLite 保存便签和访问日志。
package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

// Clip 是墙上的一张便签。时间都是 Unix 毫秒。
type Clip struct {
	ID        string `json:"id"`
	Room      string `json:"-"`
	Seq       int64  `json:"no"`
	Kind      string `json:"kind"` // text | image | file
	Content   string `json:"content,omitempty"`
	FileKey   string `json:"-"`
	ThumbKey  string `json:"-"`
	HasThumb  bool   `json:"hasThumb,omitempty"`
	FileName  string `json:"fileName,omitempty"`
	Mime      string `json:"mime,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Pinned    bool   `json:"pinned"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
	CreatorIP string `json:"creatorIp"`
	Views     int64  `json:"views"`
	Copies    int64  `json:"copies"`
	Downloads int64  `json:"downloads"`
}

type LogEntry struct {
	Action string `json:"action"` // create | view | copy | download | pin | unpin
	IP     string `json:"ip"`
	UA     string `json:"ua"`
	At     int64  `json:"at"`
}

// Removed 是被清理掉的便签，用于通知对应的墙。
type Removed struct {
	ID   string
	Room string
}

const schema = `
CREATE TABLE IF NOT EXISTS rooms (
	id         TEXT PRIMARY KEY,
	next_seq   INTEGER NOT NULL DEFAULT 1,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS clips (
	id             TEXT PRIMARY KEY,
	room           TEXT NOT NULL,
	seq            INTEGER NOT NULL,
	kind           TEXT NOT NULL,
	content        TEXT NOT NULL DEFAULT '',
	file_key       TEXT NOT NULL DEFAULT '',
	thumb_key      TEXT NOT NULL DEFAULT '',
	file_name      TEXT NOT NULL DEFAULT '',
	mime           TEXT NOT NULL DEFAULT '',
	size           INTEGER NOT NULL DEFAULT 0,
	pinned         INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL,
	expires_at     INTEGER NOT NULL,
	creator_ip     TEXT NOT NULL DEFAULT '',
	creator_ua     TEXT NOT NULL DEFAULT '',
	view_count     INTEGER NOT NULL DEFAULT 0,
	copy_count     INTEGER NOT NULL DEFAULT 0,
	download_count INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS clips_room ON clips(room, pinned DESC, created_at DESC);
CREATE INDEX IF NOT EXISTS clips_expiry ON clips(pinned, expires_at);
CREATE TABLE IF NOT EXISTS logs (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	clip_id TEXT NOT NULL,
	action  TEXT NOT NULL,
	ip      TEXT NOT NULL,
	ua      TEXT NOT NULL DEFAULT '',
	at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS logs_clip ON logs(clip_id, at);
`

const clipCols = `id, room, seq, kind, content, file_key, thumb_key, file_name, mime, size, pinned,
	created_at, expires_at, creator_ip, view_count, copy_count, download_count`

type DB struct{ sql *sql.DB }

func Open(path string) (*DB, error) {
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// 数据量很小，单连接串行访问即可，彻底避免 SQLITE_BUSY。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{db}, nil
}

func (d *DB) Close() error { return d.sql.Close() }

type scanner interface{ Scan(...any) error }

func scanClip(s scanner) (*Clip, error) {
	var c Clip
	err := s.Scan(&c.ID, &c.Room, &c.Seq, &c.Kind, &c.Content, &c.FileKey, &c.ThumbKey, &c.FileName,
		&c.Mime, &c.Size, &c.Pinned, &c.CreatedAt, &c.ExpiresAt, &c.CreatorIP,
		&c.Views, &c.Copies, &c.Downloads)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.HasThumb = c.ThumbKey != ""
	return &c, nil
}

// CreateClip 写入便签，并按墙分配递增编号（填回 c.Seq），同时记一条 create 日志。
func (d *DB) CreateClip(ctx context.Context, c *Clip, ua string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO rooms(id, next_seq, created_at) VALUES(?, 1, ?) ON CONFLICT(id) DO NOTHING`,
		c.Room, c.CreatedAt); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx,
		`UPDATE rooms SET next_seq = next_seq + 1 WHERE id = ? RETURNING next_seq - 1`,
		c.Room).Scan(&c.Seq); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO clips(`+clipCols+`, creator_ua)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,0,0,?)`,
		c.ID, c.Room, c.Seq, c.Kind, c.Content, c.FileKey, c.ThumbKey, c.FileName, c.Mime, c.Size,
		c.Pinned, c.CreatedAt, c.ExpiresAt, c.CreatorIP, ua); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO logs(clip_id, action, ip, ua, at) VALUES(?, 'create', ?, ?, ?)`,
		c.ID, c.CreatorIP, ua, c.CreatedAt); err != nil {
		return err
	}
	c.HasThumb = c.ThumbKey != ""
	return tx.Commit()
}

// ListClips 返回一面墙上仍然有效的便签，钉住的排在前面。
func (d *DB) ListClips(ctx context.Context, room string, now int64) ([]*Clip, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+clipCols+` FROM clips
		WHERE room = ? AND (pinned = 1 OR expires_at > ?)
		ORDER BY pinned DESC, created_at DESC LIMIT 1000`, room, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	clips := []*Clip{}
	for rows.Next() {
		c, err := scanClip(rows)
		if err != nil {
			return nil, err
		}
		clips = append(clips, c)
	}
	return clips, rows.Err()
}

// GetClip 取一张便签；已过期但还没被清理的视为不存在。
func (d *DB) GetClip(ctx context.Context, id string, now int64) (*Clip, error) {
	return getClip(ctx, d.sql, id, now)
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getClip(ctx context.Context, q querier, id string, now int64) (*Clip, error) {
	c, err := scanClip(q.QueryRowContext(ctx, `SELECT `+clipCols+` FROM clips WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if !c.Pinned && c.ExpiresAt <= now {
		return nil, ErrNotFound
	}
	return c, nil
}

var counterCol = map[string]string{
	"view":     "view_count",
	"copy":     "copy_count",
	"download": "download_count",
}

// Record 记一条访问日志并累加对应计数。dedupe > 0 时，同一 IP + UA 在该时间窗口内的
// 重复动作不再记录，此时 recorded 为 false。
func (d *DB) Record(ctx context.Context, id, action, ip, ua string, now, dedupe int64) (c *Clip, recorded bool, err error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	if c, err = getClip(ctx, tx, id, now); err != nil {
		return nil, false, err
	}
	if dedupe > 0 {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM logs
			WHERE clip_id = ? AND action = ? AND ip = ? AND ua = ? AND at >= ? LIMIT 1`,
			id, action, ip, ua, now-dedupe).Scan(&one)
		if err == nil {
			return c, false, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, false, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO logs(clip_id, action, ip, ua, at) VALUES(?, ?, ?, ?, ?)`,
		id, action, ip, ua, now); err != nil {
		return nil, false, err
	}
	if col, ok := counterCol[action]; ok {
		if _, err := tx.ExecContext(ctx,
			`UPDATE clips SET `+col+` = `+col+` + 1 WHERE id = ?`, id); err != nil {
			return nil, false, err
		}
	}
	if c, err = getClip(ctx, tx, id, now); err != nil {
		return nil, false, err
	}
	return c, true, tx.Commit()
}

// SetPinned 钉住或取消钉住。取消钉住时，有效期至少再延长 ttl，避免刚取消就被清理。
func (d *DB) SetPinned(ctx context.Context, id string, pinned bool, ttl, now int64, ip, ua string) (*Clip, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	c, err := getClip(ctx, tx, id, now)
	if err != nil {
		return nil, err
	}
	if c.Pinned == pinned {
		return c, nil
	}
	expires, action := c.ExpiresAt, "pin"
	if !pinned {
		expires, action = max(c.ExpiresAt, now+ttl), "unpin"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE clips SET pinned = ?, expires_at = ? WHERE id = ?`,
		pinned, expires, id); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO logs(clip_id, action, ip, ua, at) VALUES(?, ?, ?, ?, ?)`,
		id, action, ip, ua, now); err != nil {
		return nil, err
	}
	if c, err = getClip(ctx, tx, id, now); err != nil {
		return nil, err
	}
	return c, tx.Commit()
}

// DeleteClip 删除便签及其日志。只删数据库记录，图片文件保留。
func (d *DB) DeleteClip(ctx context.Context, id string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM logs WHERE clip_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM clips WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteExpired 清理过期且未钉住的便签（只删记录，不删图片文件）。
func (d *DB) DeleteExpired(ctx context.Context, now int64) ([]Removed, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id, room FROM clips WHERE pinned = 0 AND expires_at <= ?`, now)
	if err != nil {
		return nil, err
	}
	var removed []Removed
	for rows.Next() {
		var r Removed
		if err := rows.Scan(&r.ID, &r.Room); err != nil {
			rows.Close()
			return nil, err
		}
		removed = append(removed, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(removed) == 0 {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM logs WHERE clip_id IN
		(SELECT id FROM clips WHERE pinned = 0 AND expires_at <= ?)`, now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM clips WHERE pinned = 0 AND expires_at <= ?`, now); err != nil {
		return nil, err
	}
	return removed, tx.Commit()
}

// Logs 返回一张便签最近的访问日志，新的在前。
func (d *DB) Logs(ctx context.Context, id string, limit int) ([]LogEntry, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT action, ip, ua, at FROM logs WHERE clip_id = ? ORDER BY id DESC LIMIT ?`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs := []LogEntry{}
	for rows.Next() {
		var l LogEntry
		if err := rows.Scan(&l.Action, &l.IP, &l.UA, &l.At); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}
