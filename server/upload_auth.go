package server

import (
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
)

// Bound multipart headers as well as field values before authentication: Go's
// default MIME-header limit alone can otherwise buffer megabytes anonymously.
// After a leading token is verified, the existing 21MiB total limit still applies.
type uploadAuthBody struct {
	io.ReadCloser
	limited    *io.LimitedReader
	authorized bool
}

func (b *uploadAuthBody) Read(p []byte) (int, error) {
	if b.authorized {
		return b.ReadCloser.Read(p)
	}
	return b.limited.Read(p)
}

// Header/query authentication never reads the image body. Legacy multipart
// clients must place token/auth_token before file; buffering a file in order to
// find a trailing password would recreate the anonymous memory/slot exhaustion.
func (s *Server) authorizeUpload(w http.ResponseWriter, r *http.Request) (*multipart.Reader, bool) {
	if !s.authAttemptAllowed(w, r) {
		return nil, false
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		s.jsonResp(w, http.StatusBadRequest, "请使用 multipart/form-data 上传图片", nil)
		return nil, false
	}
	valid := s.checkDataAuth(r)
	if valid || hasAuthCredential(r) {
		if !s.recordAuthResult(w, r, valid) {
			return nil, false
		}
		if !valid {
			s.jsonResp(w, http.StatusUnauthorized, "未授权访问", nil)
		}
		return nil, valid
	}
	guard := &uploadAuthBody{ReadCloser: r.Body, limited: &io.LimitedReader{R: r.Body, N: 16 * 1024}}
	r.Body = guard
	reader, err := r.MultipartReader()
	if err != nil {
		s.jsonResp(w, http.StatusBadRequest, "解析上传表单失败", nil)
		return nil, false
	}
	r.Form = make(url.Values)
	remaining := int64(8192)
	for fields := 0; fields < 8; fields++ {
		part, err := reader.NextPart()
		if err != nil || part.FileName() != "" {
			break
		}
		value, err := io.ReadAll(io.LimitReader(part, remaining+1))
		remaining -= int64(len(value))
		if err != nil || remaining < 0 {
			break
		}
		r.Form.Add(part.FormName(), string(value))
		if part.FormName() == "token" || part.FormName() == "auth_token" {
			valid = s.checkDataAuth(r)
			if !s.recordAuthResult(w, r, valid) {
				return nil, false
			}
			if valid {
				guard.authorized = true
				return reader, true
			}
			break
		}
	}
	s.jsonResp(w, http.StatusUnauthorized, "请使用鉴权请求头，或将 token 字段放在 file 之前", nil)
	return nil, false
}

func parseUploadForm(r *http.Request, reader *multipart.Reader) error {
	if reader == nil {
		return r.ParseMultipartForm(1 << 60)
	}
	form, err := reader.ReadForm(1 << 60)
	if err != nil {
		return err
	}
	for key, values := range r.Form {
		form.Value[key] = append(values, form.Value[key]...)
	}
	r.MultipartForm = form
	r.PostForm = url.Values(form.Value)
	r.Form = r.PostForm
	return nil
}
