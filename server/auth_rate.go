package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const authLockDuration = 10 * time.Minute
const maxAuthRateRecords = 4096

func (s *Server) authRateReply(w http.ResponseWriter, seconds int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	s.jsonResp(w, http.StatusTooManyRequests, fmt.Sprintf("尝试次数过多，请在 %d 秒后再试", seconds), nil)
}

// All credential oracles share the same IP bucket, including status and APIs.
// Check before reading upload bodies or comparing credentials; check again when
// recording the result so concurrent attempts cannot get past an active lock.
func (s *Server) authAttemptAllowed(w http.ResponseWriter, r *http.Request) bool {
	now, ip := time.Now(), s.getClientIP(r)
	s.mu.Lock()
	if !now.Before(s.authPruneAt) {
		for key, rec := range s.authRates {
			if !now.Before(rec.LockedUntil) && now.Sub(rec.LastSeen) >= authLockDuration {
				delete(s.authRates, key)
			}
		}
		s.authPruneAt = now.Add(time.Minute)
	}
	rec := s.authRates[ip]
	seconds := 0
	if rec != nil && now.Before(rec.LockedUntil) {
		seconds = int(rec.LockedUntil.Sub(now).Seconds()) + 1
	} else if rec == nil && len(s.authRates) >= maxAuthRateRecords {
		seconds = 60
	}
	s.mu.Unlock()
	if seconds > 0 {
		s.authRateReply(w, seconds)
		return false
	}
	return true
}

func (s *Server) recordAuthResult(w http.ResponseWriter, r *http.Request, valid bool) bool {
	now, ip := time.Now(), s.getClientIP(r)
	s.mu.Lock()
	rec := s.authRates[ip]
	if rec != nil && now.Before(rec.LockedUntil) {
		seconds := int(rec.LockedUntil.Sub(now).Seconds()) + 1
		s.mu.Unlock()
		s.authRateReply(w, seconds)
		return false
	}
	if valid {
		delete(s.authRates, ip)
		s.mu.Unlock()
		return true
	}
	if rec == nil {
		if len(s.authRates) >= maxAuthRateRecords {
			s.mu.Unlock()
			s.authRateReply(w, 60)
			return false
		}
		rec = &RateRecord{}
		s.authRates[ip] = rec
	} else if now.Sub(rec.LastSeen) >= authLockDuration {
		rec.Failures = 0
	}
	rec.LastSeen = now
	rec.Failures++
	if rec.Failures >= 5 {
		rec.LockedUntil = now.Add(authLockDuration)
	}
	s.mu.Unlock()
	return true
}

func hasAuthCredential(r *http.Request) bool {
	for _, header := range []string{"Authorization", "X-API-Key", "X-Auth-Token", "X-Upload-Token"} {
		if r.Header.Get(header) != "" {
			return true
		}
	}
	return r.Form.Get("token") != "" || r.Form.Get("auth_token") != ""
}

func (s *Server) authRateMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if !strings.HasPrefix(path, "/api/") || path == "/api/upload" || path == "/api/auth/verify" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if !s.authAttemptAllowed(w, r) {
			return
		}
		valid := s.checkAuth(r)
		if path == "/api/objects" || path == "/api/share-links" || strings.HasPrefix(path, "/api/download/") {
			valid = s.checkDataAuth(r)
		}
		// Anonymous bootstrap is not a password guess. Invalid credentials on
		// status still consume the same budget as a failed login.
		if hasAuthCredential(r) && !s.recordAuthResult(w, r, valid) {
			return
		}
		if !valid && path != "/api/status" {
			s.jsonResp(w, http.StatusUnauthorized, "未授权", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}
