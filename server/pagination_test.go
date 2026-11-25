package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/3922510-creator/pic-tg/storage"
)

func TestHandleListObjectsPagination(t *testing.T) {
	s, cleanup := setupTestServer(t)
	defer cleanup()
	s.cfg.APIKey = "key-456"

	rows := []*storage.ObjectRecord{
		{Key: "telegram/c1/2026/11/02/p1.png", FID: "f1", FileID: "u1", Size: 2048, OrigSize: 4096, Width: 100, Height: 100, Mime: "image/png", CreatedAt: "2026-11-02 18:00:00"},
		{Key: "telegram/c1/2026/11/02/p2.jpg", FID: "f2", FileID: "u2", Size: 8192, OrigSize: 8192, Width: 200, Height: 200, Mime: "image/jpeg", CreatedAt: "2026-11-02 10:00:00"},
		{Key: "telegram/c1/2026/11/01/p3.webp", FID: "f3", FileID: "u3", Size: 1024, OrigSize: 1024, Width: 50, Height: 50, Mime: "image/webp", CreatedAt: "2026-11-01 08:00:00"},
	}
	for _, row := range rows {
		if err := s.db.SaveObject(row); err != nil {
			t.Fatal(err)
		}
	}

	type listResp struct {
		Code int `json:"code"`
		Data struct {
			List []struct {
				Key  string `json:"key"`
				Size int64  `json:"size"`
			} `json:"list"`
			Total      int   `json:"total"`
			TotalSize  int64 `json:"total_size"`
			HasMore    bool  `json:"has_more"`
			NextOffset int   `json:"next_offset"`
			Summary    *struct {
				TotalCount int `json:"total_count"`
				TotalSize  int64 `json:"total_size"`
				Dates      []struct {
					Date  string `json:"date"`
					Count int    `json:"count"`
				} `json:"dates"`
			} `json:"summary"`
		} `json:"data"`
	}

	fetch := func(target string) listResp {
		t.Helper()
		r := httptest.NewRequest("GET", target, nil)
		r.Header.Set("Authorization", "Bearer key-456")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s -> HTTP %d: %s", target, w.Code, w.Body.String())
		}
		var out listResp
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", target, err)
		}
		return out
	}

	// Page 1 sorted by size descending with summary.
	p1 := fetch("/api/objects?limit=2&offset=0&sort=big&tz_offset=480&summary=1")
	if p1.Data.Total != 3 || p1.Data.TotalSize != 11264 || !p1.Data.HasMore || p1.Data.NextOffset != 2 {
		t.Fatalf("unexpected page 1 metadata: %+v", p1.Data)
	}
	if len(p1.Data.List) != 2 || p1.Data.List[0].Key != rows[1].Key || p1.Data.List[1].Key != rows[0].Key {
		t.Fatalf("unexpected page 1 items: %+v", p1.Data.List)
	}
	if p1.Data.Summary == nil || p1.Data.Summary.TotalCount != 3 || len(p1.Data.Summary.Dates) != 3 {
		t.Fatalf("unexpected summary: %+v", p1.Data.Summary)
	}

	// Page 2 without summary.
	p2 := fetch("/api/objects?limit=2&offset=2&sort=big&tz_offset=480")
	if p2.Data.HasMore || p2.Data.NextOffset != 3 || len(p2.Data.List) != 1 || p2.Data.List[0].Key != rows[2].Key {
		t.Fatalf("unexpected page 2: %+v", p2.Data)
	}
	if p2.Data.Summary != nil {
		t.Fatalf("expected nil summary when summary param is omitted")
	}

	// Filter by local date in UTC+8.
	byDate := fetch("/api/objects?limit=10&offset=0&date=2026-11-03&tz_offset=480")
	if byDate.Data.Total != 1 || byDate.Data.HasMore || len(byDate.Data.List) != 1 || byDate.Data.List[0].Key != rows[0].Key {
		t.Fatalf("unexpected date filter response: %+v", byDate.Data)
	}
}
