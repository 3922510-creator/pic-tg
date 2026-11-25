package server

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func authProbe(s *Server, method, path, token, body, ip string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = ip + ":1234"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if path == "/api/upload" {
		r.Header.Set("Content-Type", "multipart/form-data; boundary=fixture")
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestAuthFailuresShareAllCredentialOracles(t *testing.T) {
	s, done := setupTestServer(t)
	defer done()
	ip := "192.0.2.10"
	probes := []struct {
		method, path, body string
		code               int
	}{
		{"POST", "/api/auth/verify", `{"token":"wrong"}`, 401},
		{"GET", "/api/status?auth_only=1&token=wrong", "", 200},
		{"GET", "/api/objects", "", 401},
		{"GET", "/api/admin/telegram", "", 401},
		{"POST", "/api/upload", "not read", 401},
	}
	for _, p := range probes {
		if w := authProbe(s, p.method, p.path, "wrong", p.body, ip); w.Code != p.code {
			t.Fatalf("before lock %s: %d %s", p.path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/api/auth/verify", "/api/status?auth_only=1", "/api/objects", "/api/admin/api-key", "/api/share-links", "/api/download/invalid", "/api/upload"} {
		for _, token := range []string{"wrong", "test-pass-123"} {
			method := "GET"
			if path == "/api/auth/verify" || path == "/api/upload" || path == "/api/share-links" {
				method = "POST"
			}
			w := authProbe(s, method, path, token, `{"token":"test-pass-123"}`, ip)
			if w.Code != 429 || w.Header().Get("Retry-After") == "" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("lock bypass %s %s: %d %s", method, path, w.Code, w.Body.String())
			}
		}
	}
	if w := authProbe(s, "GET", "/api/objects", "test-pass-123", "", "192.0.2.11"); w.Code != 200 {
		t.Fatal("independent IP blocked", w.Code)
	}
	if w := authProbe(s, "GET", "/", "", "", ip); w.Code != 200 {
		t.Fatal("public UI blocked")
	}
	s.mu.Lock()
	s.authRates[ip].LockedUntil = time.Now().Add(-time.Second)
	s.authRates[ip].LastSeen = time.Now().Add(-authLockDuration - time.Second)
	s.mu.Unlock()
	if w := authProbe(s, "GET", "/api/objects", "test-pass-123", "", ip); w.Code != 200 {
		t.Fatal("expired lock did not recover", w.Code)
	}
}

func TestAnonymousBootstrapAndSuccessfulAuthDoNotAccumulateFailures(t *testing.T) {
	s, done := setupTestServer(t)
	defer done()
	ip := "192.0.2.20"
	for range 12 {
		if w := authProbe(s, "GET", "/api/status?auth_only=1", "", "", ip); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if len(s.authRates) != 0 {
		t.Fatal("anonymous bootstrap counted as guesses")
	}
	for range 3 {
		authProbe(s, "GET", "/api/objects", "wrong", "", ip)
	}
	if w := authProbe(s, "GET", "/api/objects", "test-pass-123", "", ip); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if len(s.authRates) != 0 {
		t.Fatal("successful authentication did not clear failures")
	}
}

func TestConcurrentAuthFailuresCannotExceedBudget(t *testing.T) {
	s, done := setupTestServer(t)
	defer done()
	var wg sync.WaitGroup
	codes := make(chan int, 32)
	for range 32 {
		wg.Go(func() { codes <- authProbe(s, "GET", "/api/objects", "wrong", "", "192.0.2.30").Code })
	}
	wg.Wait()
	close(codes)
	failures := 0
	for code := range codes {
		if code == 401 {
			failures++
		} else if code != 429 {
			t.Fatal(code)
		}
	}
	if failures != 5 {
		t.Fatalf("concurrent guesses evaluated: %d, want 5", failures)
	}
}

func TestAuthRateRecordsAreBoundedAndExpire(t *testing.T) {
	s, done := setupTestServer(t)
	defer done()
	for i := range maxAuthRateRecords {
		s.authRates[fmt.Sprint(i)] = &RateRecord{LastSeen: time.Now(), Failures: 1}
	}
	if w := authProbe(s, "GET", "/api/objects", "test-pass-123", "", "192.0.2.40"); w.Code != 429 {
		t.Fatal("full map exposed auth oracle", w.Code)
	}
	if len(s.authRates) != maxAuthRateRecords {
		t.Fatal("unbounded rate map")
	}
	for _, rec := range s.authRates {
		rec.LastSeen = time.Now().Add(-2 * authLockDuration)
	}
	s.authPruneAt = time.Time{}
	if w := authProbe(s, "GET", "/api/objects", "test-pass-123", "", "192.0.2.40"); w.Code != 200 || len(s.authRates) != 0 {
		t.Fatal("stale records not pruned", w.Code, len(s.authRates))
	}
}

func TestQueryTokenRejectedAndClientIPPriority(t *testing.T) {
	s, done := setupTestServer(t)
	defer done()
	if w := authProbe(s, "GET", "/api/objects?token=test-pass-123", "", "", "192.0.2.50"); w.Code != 401 {
		t.Fatalf("query token accepted: %d", w.Code)
	}
	if w := authProbe(s, "GET", "/api/status?auth_only=1&token=wrong", "", "", "192.0.2.50"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if len(s.authRates) != 0 {
		t.Fatal("ignored query token should not count as credential attempt")
	}

	// Default config does not trust any proxy header, even from loopback.
	rDefault := httptest.NewRequest("GET", "/api/objects", nil)
	rDefault.RemoteAddr = "127.0.0.1:4321"
	rDefault.Header.Set("CF-Connecting-IP", "198.51.100.7")
	rDefault.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := s.getClientIP(rDefault); got != "127.0.0.1" {
		t.Fatalf("proxy header trusted when trust_proxy_header is unset: %s", got)
	}

	s.cfg.TrustProxyHeader = "CF-Connecting-IP"
	r := httptest.NewRequest("GET", "/api/objects", nil)
	r.RemoteAddr = "127.0.0.1:4321"
	r.Header.Set("CF-Connecting-IP", "198.51.100.7")
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 172.70.1.1")
	if got := s.getClientIP(r); got != "198.51.100.7" {
		t.Fatalf("CF-Connecting-IP ignored: %s", got)
	}

	s.cfg.TrustProxyHeader = "X-Real-IP"
	rReal := httptest.NewRequest("GET", "/api/objects", nil)
	rReal.RemoteAddr = "127.0.0.1:4321"
	rReal.Header.Set("X-Real-IP", "198.51.100.8")
	if got := s.getClientIP(rReal); got != "198.51.100.8" {
		t.Fatalf("X-Real-IP ignored: %s", got)
	}

	s.cfg.TrustProxyHeader = "X-Forwarded-For"
	r2 := httptest.NewRequest("GET", "/api/objects", nil)
	r2.RemoteAddr = "127.0.0.1:4321"
	r2.Header.Set("X-Forwarded-For", "203.0.113.9, 198.51.100.12")
	if got := s.getClientIP(r2); got != "198.51.100.12" {
		t.Fatalf("X-Forwarded-For should prefer right-most non-private hop over spoofable left-most hop: %s", got)
	}
	r2Priv := httptest.NewRequest("GET", "/api/objects", nil)
	r2Priv.RemoteAddr = "127.0.0.1:4321"
	r2Priv.Header.Set("X-Forwarded-For", "198.51.100.12, 10.0.0.2")
	if got := s.getClientIP(r2Priv); got != "198.51.100.12" {
		t.Fatalf("X-Forwarded-For did not skip internal proxy hop: %s", got)
	}

	s.cfg.TrustProxyHeader = "CF-Connecting-IP"
	r3 := httptest.NewRequest("GET", "/api/objects", nil)
	r3.RemoteAddr = "192.0.2.99:4321"
	r3.Header.Set("CF-Connecting-IP", "198.51.100.7")
	if got := s.getClientIP(r3); got != "192.0.2.99" {
		t.Fatalf("untrusted non-loopback proxy header accepted: %s", got)
	}
}
