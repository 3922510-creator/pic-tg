package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	CREATE INDEX IF NOT EXISTS idx_objects_size ON objects (size DESC, created_at DESC);
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

type ListOptions struct {
	Limit           int
	Offset          int
	Query           string
	Sort            string
	Date            string
	TZOffsetMinutes int
	IncludeSummary  bool
}

type DateSummary struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

type GallerySummary struct {
	TotalCount int           `json:"total_count"`
	TotalSize  int64         `json:"total_size"`
	Dates      []DateSummary `json:"dates"`
}

type ListResult struct {
	Items     []ObjectRecord
	Total     int
	TotalSize int64
	Summary   *GallerySummary
}

func clampTZOffset(minutes int) int {
	if minutes < -840 {
		return -840
	}
	if minutes > 840 {
		return 840
	}
	return minutes
}

func sqliteTZModifier(minutes int) string {
	return fmt.Sprintf("%+d minutes", clampTZOffset(minutes))
}

func listOrderClause(sort string) string {
	switch sort {
	case "old":
		return "ORDER BY o.created_at ASC, o.key ASC"
	case "big":
		return "ORDER BY o.size DESC, o.created_at DESC, o.key DESC"
	case "small":
		return "ORDER BY o.size ASC, o.created_at DESC, o.key ASC"
	default:
		return "ORDER BY o.created_at DESC, o.key DESC"
	}
}

func (d *DB) gallerySummary(tzModifier string) (*GallerySummary, error) {
	summary := &GallerySummary{
		Dates: []DateSummary{},
	}
	if err := d.QueryRow("SELECT COUNT(*), COALESCE(SUM(size), 0) FROM objects").Scan(&summary.TotalCount, &summary.TotalSize); err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT date(created_at, ?) AS local_date, COUNT(*)
		FROM objects
		WHERE created_at != ''
		GROUP BY local_date
		HAVING local_date IS NOT NULL
		ORDER BY local_date DESC`, tzModifier)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ds DateSummary
		if err := rows.Scan(&ds.Date, &ds.Count); err != nil {
			return nil, err
		}
		summary.Dates = append(summary.Dates, ds)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return summary, nil
}

func (d *DB) ListObjectsPage(opts ListOptions) (*ListResult, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}

	tzMinutes := clampTZOffset(opts.TZOffsetMinutes)
	tzModifier := sqliteTZModifier(tzMinutes)

	result := &ListResult{
		Items: []ObjectRecord{},
	}
	if opts.IncludeSummary {
		summary, err := d.gallerySummary(tzModifier)
		if err != nil {
			return nil, err
		}
		result.Summary = summary
	}

	var whereParts []string
	var args []any

	if dateStr := strings.TrimSpace(opts.Date); dateStr != "" {
		dayStart, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			return result, nil
		}
		// Convert the client-local calendar day [00:00, +24h) into UTC bounds so idx_objects_created can be used.
		utcStart := dayStart.Add(-time.Duration(tzMinutes) * time.Minute)
		utcEnd := utcStart.Add(24 * time.Hour)
		whereParts = append(whereParts, "o.created_at >= ? AND o.created_at < ?")
		args = append(args, utcStart.Format("2006-01-02 15:04:05"), utcEnd.Format("2006-01-02 15:04:05"))
	}

	if query := strings.TrimSpace(opts.Query); query != "" {
		likePattern := "%" + likeEscaper.Replace(query) + "%"
		whereParts = append(whereParts, `(o.key LIKE ? ESCAPE '\' OR date(o.created_at, ?) LIKE ? ESCAPE '\')`)
		args = append(args, likePattern, tzModifier, likePattern)
	}

	whereSQL := ""
	if len(whereParts) > 0 {
		whereSQL = " WHERE " + strings.Join(whereParts, " AND ")
	}

	if err := d.QueryRow("SELECT COUNT(*), COALESCE(SUM(o.size), 0) FROM objects o"+whereSQL, args...).Scan(&result.Total, &result.TotalSize); err != nil {
		return nil, err
	}

	querySQL := `SELECT o.key, o.fid, o.file_id, o.size, o.orig_size, o.width, o.height, o.format, o.mime, o.created_at, o.backend, o.tg_chat_id, o.tg_message_id, o.connection_id, COALESCE(p.token, '')
		FROM objects o
		LEFT JOIN public_links p ON p.object_key = o.key` + whereSQL + " " + listOrderClause(opts.Sort) + " LIMIT ? OFFSET ?"
	queryArgs := append(append([]any{}, args...), limit, offset)

	rows, err := d.Query(querySQL, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var o ObjectRecord
		if err := rows.Scan(&o.Key, &o.FID, &o.FileID, &o.Size, &o.OrigSize, &o.Width, &o.Height, &o.Format, &o.Mime, &o.CreatedAt, &o.Backend, &o.TGChatID, &o.TGMessageID, &o.ConnectionID, &o.PublicToken); err != nil {
			return nil, err
		}
		result.Items = append(result.Items, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (d *DB) ListObjects(limit, offset int, query string) ([]ObjectRecord, int, error) {
	res, err := d.ListObjectsPage(ListOptions{
		Limit:  limit,
		Offset: offset,
		Query:  query,
	})
	if err != nil {
		return nil, 0, err
	}
	return res.Items, res.Total, nil
}
