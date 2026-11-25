package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

type ObjectRecord struct {
	Key          string `json:"key"`
	FID          string `json:"fid"`
	FileID       string `json:"file_id"`
	Size         int64  `json:"size"`
	OrigSize     int64  `json:"orig_size"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Format       string `json:"format"`
	Mime         string `json:"mime"`
	CreatedAt    string `json:"created_at"`
	Backend      string `json:"backend"`
	TGChatID     int64  `json:"tg_chat_id,omitempty"`
	ConnectionID string `json:"-"`
	TGMessageID  int64  `json:"tg_message_id,omitempty"`
	PublicToken  string `json:"-"`
}

type DB struct {
	*sql.DB
}

func OpenDB(dirPath string) (*DB, error) {
	if err := os.MkdirAll(dirPath, 0700); err != nil {
		return nil, fmt.Errorf("mkdir db dir failed: %w", err)
	}

	dbFile := filepath.Join(dirPath, "objects.sqlite3")
	db, err := sql.Open("sqlite", dbFile+"?_busy_timeout=30000&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open sqlite failed: %w", err)
	}

	schema := `
	CREATE TABLE IF NOT EXISTS objects (
		key TEXT PRIMARY KEY,
		fid TEXT NOT NULL,
		file_id TEXT NOT NULL,
		size INTEGER,
		orig_size INTEGER DEFAULT 0,
		width INTEGER DEFAULT 0,
		height INTEGER DEFAULT 0,
		format TEXT DEFAULT '',
		mime TEXT,
		created_at TEXT,
		connection_id TEXT NOT NULL DEFAULT '',
		backend TEXT NOT NULL DEFAULT 'telegram',
		tg_chat_id INTEGER NOT NULL DEFAULT 0,
		tg_message_id INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE IF NOT EXISTS public_links (
		object_key TEXT PRIMARY KEY,
		token TEXT NOT NULL UNIQUE
	);
	CREATE INDEX IF NOT EXISTS idx_objects_created ON objects (created_at DESC);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("exec schema failed: %w", err)
	}

	// Additive column migrations for older SQLite databases.
	columns := map[string]string{
		"orig_size":     "INTEGER DEFAULT 0",
		"width":         "INTEGER DEFAULT 0",
		"height":        "INTEGER DEFAULT 0",
		"format":        "TEXT DEFAULT ''",
		"connection_id": "TEXT NOT NULL DEFAULT ''",
		"backend":       "TEXT NOT NULL DEFAULT 'telegram'",
		"tg_chat_id":    "INTEGER NOT NULL DEFAULT 0",
		"tg_message_id": "INTEGER NOT NULL DEFAULT 0",
	}
	rows, err := db.Query("PRAGMA table_info(objects)")
	if err != nil {
		db.Close()
		return nil, err
	}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			rows.Close()
			db.Close()
			return nil, err
		}
		delete(columns, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		db.Close()
		return nil, err
	}
	rows.Close()
	for name, declaration := range columns {
		if _, err := db.Exec("ALTER TABLE objects ADD COLUMN " + name + " " + declaration); err != nil {
			db.Close()
			return nil, fmt.Errorf("schema migration failed for column %s: %w", name, err)
		}
	}
	return &DB{DB: db}, nil
}

func (d *DB) GetObject(key string) (*ObjectRecord, error) {
	var obj ObjectRecord
	err := d.QueryRow(`SELECT key, fid, file_id, size, orig_size, width, height, format, mime, created_at, backend, tg_chat_id, tg_message_id, connection_id
		FROM objects WHERE key = ?`, key).
		Scan(&obj.Key, &obj.FID, &obj.FileID, &obj.Size, &obj.OrigSize, &obj.Width, &obj.Height, &obj.Format, &obj.Mime, &obj.CreatedAt, &obj.Backend, &obj.TGChatID, &obj.TGMessageID, &obj.ConnectionID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &obj, nil
}

func (d *DB) SaveObject(obj *ObjectRecord) error {
	if obj.Backend == "" {
		obj.Backend = "telegram"
	}
	_, err := d.Exec(`INSERT OR REPLACE INTO objects
		(key, fid, file_id, size, orig_size, width, height, format, mime, created_at, backend, tg_chat_id, tg_message_id, connection_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		obj.Key, obj.FID, obj.FileID, obj.Size, obj.OrigSize, obj.Width, obj.Height, obj.Format, obj.Mime, obj.CreatedAt, obj.Backend, obj.TGChatID, obj.TGMessageID, obj.ConnectionID)
	return err
}

func (d *DB) DeleteObject(key string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM objects WHERE key = ?", key); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM public_links WHERE object_key = ?", key); err != nil {
		return err
	}
	return tx.Commit()
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (d *DB) ListObjects(limit, offset int, query string) ([]ObjectRecord, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	var rows *sql.Rows
	var err error

	if query != "" {
		likePattern := "%" + likeEscaper.Replace(query) + "%"
		if err := d.QueryRow(`SELECT COUNT(*) FROM objects WHERE key LIKE ? ESCAPE '\'`, likePattern).Scan(&total); err != nil {
			return nil, 0, err
		}
		rows, err = d.Query(`SELECT o.key, o.fid, o.file_id, o.size, o.orig_size, o.width, o.height, o.format, o.mime, o.created_at, o.backend, o.tg_chat_id, o.tg_message_id, o.connection_id, COALESCE(p.token, '')
			FROM objects o
			LEFT JOIN public_links p ON p.object_key = o.key
			WHERE o.key LIKE ? ESCAPE '\'
			ORDER BY o.created_at DESC LIMIT ? OFFSET ?`, likePattern, limit, offset)
	} else {
		if err := d.QueryRow("SELECT COUNT(*) FROM objects").Scan(&total); err != nil {
			return nil, 0, err
		}
		rows, err = d.Query(`SELECT o.key, o.fid, o.file_id, o.size, o.orig_size, o.width, o.height, o.format, o.mime, o.created_at, o.backend, o.tg_chat_id, o.tg_message_id, o.connection_id, COALESCE(p.token, '')
			FROM objects o
			LEFT JOIN public_links p ON p.object_key = o.key
			ORDER BY o.created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	}
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var list []ObjectRecord
	for rows.Next() {
		var o ObjectRecord
		if err := rows.Scan(&o.Key, &o.FID, &o.FileID, &o.Size, &o.OrigSize, &o.Width, &o.Height, &o.Format, &o.Mime, &o.CreatedAt, &o.Backend, &o.TGChatID, &o.TGMessageID, &o.ConnectionID, &o.PublicToken); err != nil {
			return nil, 0, err
		}
		list = append(list, o)
	}
	return list, total, rows.Err()
}
