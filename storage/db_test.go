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

func TestListObjectsPagePaginationSortDateAndSummary(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	records := []*ObjectRecord{
		// In UTC+8 (+480m), 2026-11-02 18:00:00 UTC is 2026-11-03 02:00:00.
		{Key: "telegram/conn/2026/11/02/a-small.png", FID: "f1", FileID: "u1", Size: 100, CreatedAt: "2026-11-02 18:00:00"},
		// Same timestamp as next item to verify deterministic tie-break ordering across pages.
		{Key: "telegram/conn/2026/11/02/b-mid.png", FID: "f2", FileID: "u2", Size: 300, CreatedAt: "2026-11-02 10:00:00"},
		{Key: "telegram/conn/2026/11/02/c-big.png", FID: "f3", FileID: "u3", Size: 900, CreatedAt: "2026-11-02 10:00:00"},
		{Key: "telegram/conn/2026/11/01/d-old.jpg", FID: "f4", FileID: "u4", Size: 500, CreatedAt: "2026-11-01 05:00:00"},
	}
	for _, r := range records {
		if err := db.SaveObject(r); err != nil {
			t.Fatal(err)
		}
	}

	// Page 1 & Page 2 with summary in UTC+8.
	p1, err := db.ListObjectsPage(ListOptions{Limit: 2, Offset: 0, Sort: "new", TZOffsetMinutes: 480, IncludeSummary: true})
	if err != nil {
		t.Fatal(err)
	}
	if p1.Total != 4 || p1.TotalSize != 1800 || len(p1.Items) != 2 {
		t.Fatalf("unexpected page 1: %+v", p1)
	}
	if p1.Items[0].Key != records[0].Key || p1.Items[1].Key != records[2].Key {
		t.Fatalf("unexpected page 1 order: %s, %s", p1.Items[0].Key, p1.Items[1].Key)
	}
	if p1.Summary == nil || p1.Summary.TotalCount != 4 || p1.Summary.TotalSize != 1800 {
		t.Fatalf("unexpected summary: %+v", p1.Summary)
	}
	if len(p1.Summary.Dates) != 3 ||
		p1.Summary.Dates[0] != (DateSummary{Date: "2026-11-03", Count: 1}) ||
		p1.Summary.Dates[1] != (DateSummary{Date: "2026-11-02", Count: 2}) ||
		p1.Summary.Dates[2] != (DateSummary{Date: "2026-11-01", Count: 1}) {
		t.Fatalf("unexpected local-date summary: %+v", p1.Summary.Dates)
	}

	p2, err := db.ListObjectsPage(ListOptions{Limit: 2, Offset: 2, Sort: "new", TZOffsetMinutes: 480})
	if err != nil {
		t.Fatal(err)
	}
	if p2.Summary != nil {
		t.Fatalf("summary should be omitted when IncludeSummary=false")
	}
	if len(p2.Items) != 2 || p2.Items[0].Key != records[1].Key || p2.Items[1].Key != records[3].Key {
		t.Fatalf("unexpected page 2 items: %+v", p2.Items)
	}

	// Sort by size desc and asc.
	big, err := db.ListObjectsPage(ListOptions{Limit: 4, Offset: 0, Sort: "big"})
	if err != nil {
		t.Fatal(err)
	}
	if big.Items[0].Size != 900 || big.Items[3].Size != 100 {
		t.Fatalf("big sort mismatch: %+v", big.Items)
	}
	small, err := db.ListObjectsPage(ListOptions{Limit: 4, Offset: 0, Sort: "small"})
	if err != nil {
		t.Fatal(err)
	}
	if small.Items[0].Size != 100 || small.Items[3].Size != 900 {
		t.Fatalf("small sort mismatch: %+v", small.Items)
	}
	oldest, err := db.ListObjectsPage(ListOptions{Limit: 4, Offset: 0, Sort: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if oldest.Items[0].Key != records[3].Key || oldest.Items[3].Key != records[0].Key {
		t.Fatalf("old sort mismatch: %+v", oldest.Items)
	}

	// Local date filter in UTC+8 should find the item uploaded at 18:00 UTC under 2026-11-03.
	byDate, err := db.ListObjectsPage(ListOptions{Limit: 10, Offset: 0, Date: "2026-11-03", TZOffsetMinutes: 480, IncludeSummary: true})
	if err != nil {
		t.Fatal(err)
	}
	if byDate.Total != 1 || byDate.TotalSize != 100 || len(byDate.Items) != 1 || byDate.Items[0].Key != records[0].Key {
		t.Fatalf("local date filter failed: %+v", byDate)
	}
	if byDate.Summary == nil || byDate.Summary.TotalCount != 4 {
		t.Fatalf("summary must reflect full gallery even under date filter: %+v", byDate.Summary)
	}

	// Search by local date string.
	byLocalDateQuery, err := db.ListObjectsPage(ListOptions{Limit: 10, Offset: 0, Query: "2026-11-03", TZOffsetMinutes: 480})
	if err != nil {
		t.Fatal(err)
	}
	if byLocalDateQuery.Total != 1 || byLocalDateQuery.Items[0].Key != records[0].Key {
		t.Fatalf("local date search failed: %+v", byLocalDateQuery)
	}

	// Invalid date returns empty result safely.
	badDate, err := db.ListObjectsPage(ListOptions{Limit: 10, Offset: 0, Date: "not-a-date"})
	if err != nil || badDate.Total != 0 || len(badDate.Items) != 0 {
		t.Fatalf("invalid date should return empty result: %+v err=%v", badDate, err)
	}
}
