package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
)

type countedBody struct {
	io.Reader
	read int
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}
func (*countedBody) Close() error { return nil }

func TestUnauthorizedUploadDoesNotReadBodyOrConsumeSlots(t *testing.T) {
	for _, contentType := range []string{"multipart/form-data; boundary=fixture", "application/x-www-form-urlencoded"} {
		s, done := setupTestServer(t)
		body := &countedBody{Reader: strings.NewReader(strings.Repeat("x", 1024*1024))}
		r := httptest.NewRequest("POST", "/api/upload", nil)
		r.Body = body
		r.Header.Set("Content-Type", contentType)
		r.Header.Set("Authorization", "Bearer wrong")
		// Even while all real upload workers are busy, reject before reading.
		for range cap(s.uploadSlots) {
			s.uploadSlots <- struct{}{}
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if (w.Code != 401 && w.Code != 400) || body.read != 0 || len(s.uploadSlots) != 3 {
			t.Fatalf("anonymous upload used resources: %d read=%d slots=%d", w.Code, body.read, len(s.uploadSlots))
		}
		done()
	}
}

func TestLegacyFormRequiresTokenBeforeFileWithBoundedPreamble(t *testing.T) {
	for _, tokenFirst := range []bool{false, true} {
		s, done := setupTestServer(t)
		var buf bytes.Buffer
		form := multipart.NewWriter(&buf)
		if tokenFirst {
			form.WriteField("token", "wrong")
		}
		part, _ := form.CreateFormFile("file", "large.png")
		part.Write(make([]byte, 1024*1024))
		if !tokenFirst {
			form.WriteField("token", "test-pass-123")
		}
		form.Close()
		body := &countedBody{Reader: bytes.NewReader(buf.Bytes())}
		r := httptest.NewRequest("POST", "/api/upload", nil)
		r.Body = body
		r.Header.Set("Content-Type", form.FormDataContentType())
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 401 || body.read > 16384 || len(s.uploadSlots) != 0 {
			t.Fatalf("unbounded pre-auth form: %d bytes=%d slots=%d", w.Code, body.read, len(s.uploadSlots))
		}
		done()
	}
}

func TestBusyAuthenticatedUploadReturnsWithoutReadingBody(t *testing.T) {
	s, done := setupTestServer(t)
	defer done()
	for range cap(s.uploadSlots) {
		s.uploadSlots <- struct{}{}
	}
	body := &countedBody{Reader: strings.NewReader("not read")}
	r := httptest.NewRequest("POST", "/api/upload", nil)
	r.Body = body
	r.Header.Set("Content-Type", "multipart/form-data; boundary=fixture")
	r.Header.Set("Authorization", "Bearer test-pass-123")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 503 || body.read != 0 || w.Header().Get("Retry-After") != "1" {
		t.Fatal(w.Code, body.read, w.Header())
	}
}

func TestAnonymousMultipartHeadersAndFieldsHaveTotalReadBudget(t *testing.T) {
	for _, preamble := range []string{
		"--fixture\r\nX-Padded: " + strings.Repeat("x", 64*1024),
		"--fixture\r\nContent-Disposition: form-data; name=\"token\"\r\n\r\n" + strings.Repeat("x", 64*1024),
	} {
		s, done := setupTestServer(t)
		body := &countedBody{Reader: strings.NewReader(preamble)}
		r := httptest.NewRequest("POST", "/api/upload", nil)
		r.Body = body
		r.Header.Set("Content-Type", "multipart/form-data; boundary=fixture")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 401 || body.read > 16*1024 || len(s.uploadSlots) != 0 {
			t.Fatal("pre-auth MIME header/value bypass", w.Code, body.read, len(s.uploadSlots))
		}
		done()
	}
}
