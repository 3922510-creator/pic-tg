package storage

import (
	"regexp"
	"sync"
	"testing"
)

func TestPublicLinksStablePrivateAndPersistent(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "telegram/0123456789abcdef/2026/11/02/abcdef123456.png"
	obj := &ObjectRecord{Key: key, FID: "f", FileID: "f", Mime: "image/png"}
	if err := db.SaveObject(obj); err != nil {
		t.Fatal(err)
	}
	token, err := db.PublicToken(key)
	if err != nil || !regexp.MustCompile(`^[a-f0-9]{32}\.png$`).MatchString(token) {
		t.Fatal(token, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := db.PublicToken(key)
			if err != nil || got != token {
				t.Errorf("unstable: %q %v", got, err)
			}
		}()
	}
	wg.Wait()
	if err := db.SaveObject(obj); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = OpenDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.PublicToken(key)
	if err != nil || got != token {
		t.Fatal(got, err)
	}
	mapped, err := db.KeyForPublicToken(token)
	if err != nil || mapped != key {
		t.Fatal(mapped, err)
	}
	if err := db.DeleteObject(key); err != nil {
		t.Fatal(err)
	}
	var linkCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM public_links WHERE object_key = ?", key).Scan(&linkCount); err != nil || linkCount != 0 {
		t.Fatalf("public_links orphan remained: %d %v", linkCount, err)
	}
	if _, err := db.KeyForPublicToken(token); err == nil {
		t.Fatal("deleted object still resolves")
	}
	if _, err := db.PublicToken("missing"); err == nil {
		t.Fatal("missing object got alias")
	}
}
