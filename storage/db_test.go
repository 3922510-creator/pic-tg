package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestSchemaMigrationAndListObjects(t *testing.T) {
	dir := t.TempDir()
	old, err := sql.Open("sqlite", filepath.Join(dir, "objects.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`CREATE TABLE objects (key TEXT PRIMARY KEY, fid TEXT NOT NULL, file_id TEXT NOT NULL, size INTEGER, orig_size INTEGER DEFAULT 0, width INTEGER DEFAULT 0, height INTEGER DEFAULT 0, format TEXT DEFAULT '', mime TEXT, created_at TEXT); INSERT INTO objects VALUES ('telegram/2026/11/02/abcdefabcdef.png','old-fid','old-file',100,200,4,3,'png','image/png','2026-11-02 00:00:00')`)
	if err != nil {
		t.Fatal(err)
	}
	old.Close()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	migrated, err := db.GetObject("telegram/2026/11/02/abcdefabcdef.png")
	if err != nil || migrated == nil || migrated.Backend != "telegram" || migrated.FID != "old-fid" || migrated.Size != 100 || migrated.OrigSize != 200 {
		t.Fatal(migrated, err)
	}
	obj := &ObjectRecord{Key: "telegram/0123456789abcdef/2026/11/02/fedcbafedcba.png", FID: "tg-id", FileID: "tg-unique", Size: 100, OrigSize: 200, Width: 4, Height: 3, Format: "png", Mime: "image/png", Backend: "telegram", ConnectionID: "0123456789abcdef", TGChatID: -100123, TGMessageID: 12}
	if err := db.SaveObject(obj); err != nil {
		t.Fatal(err)
	}
	token, err := db.PublicToken(obj.Key)
	if err != nil || token == "" {
		t.Fatal(token, err)
	}
	got, err := db.GetObject(obj.Key)
	if err != nil || got.TGChatID != obj.TGChatID || got.TGMessageID != obj.TGMessageID || got.FID != obj.FID || got.Backend != "telegram" {
		t.Fatal(got, err)
	}
	list, total, err := db.ListObjects(10, 0, "")
	if err != nil || total != 2 || len(list) != 2 {
		t.Fatal(total, list, err)
	}
	foundToken := false
	for _, item := range list {
		if item.Key == obj.Key && item.PublicToken == token {
			foundToken = true
		}
	}
	if !foundToken {
		t.Fatalf("ListObjects did not join public_links token: %+v", list)
	}
	filtered, total, err := db.ListObjects(10, 0, "telegram/0123456789abcdef/")
	if err != nil || total != 1 || filtered[0].Key != obj.Key {
		t.Fatal(filtered, total, err)
	}
	// SQLite LIKE wildcards in prefix must be escaped literally.
	escaped, total, err := db.ListObjects(10, 0, "telegram/%")
	if err != nil || total != 0 || len(escaped) != 0 {
		t.Fatalf("LIKE wildcard not escaped: total=%d list=%+v err=%v", total, escaped, err)
	}
	escapedUnderscore, total, err := db.ListObjects(10, 0, "telegram/____/")
	if err != nil || total != 0 || len(escapedUnderscore) != 0 {
		t.Fatalf("LIKE underscore wildcard not escaped: total=%d err=%v", total, err)
	}
	db.Close()
	again, err := OpenDB(dir)
	if err != nil {
		t.Fatal("migration not idempotent:", err)
	}
	again.Close()
}
