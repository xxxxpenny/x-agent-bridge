package server

// 附件上传：POST /api/v1/{rt}/sessions/{sess}/attachments（multipart，字段 "file" 可重复）
// 落盘 data/uploads/<rt>/<sess>/<id>-<sanitized-name>；返回附件元数据供发送时引用。

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"xagent-bridge/internal/core"
)

const (
	uploadMaxBytes = 50 << 20 // 单文件 50MB
	maxFilesPerReq = 10
)

func (s *Server) handleUploadAttachment(w http.ResponseWriter, r *http.Request, rt, sess string) {
	if _, ok := s.adapters[rt]; !ok {
		writeErr(w, 404, "runtime_not_found", "unknown runtime", nil)
		return
	}
	if sess == "" || strings.ContainsAny(sess, "/\\") || strings.Contains(sess, "..") {
		writeErr(w, 400, "invalid_request", "bad session id", nil)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, uploadMaxBytes*maxFilesPerReq+(1<<20))
	if err := r.ParseMultipartForm(uploadMaxBytes << 1); err != nil {
		writeErr(w, 413, "too_large", "upload too large (single file max 50MB)", nil)
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeErr(w, 400, "invalid_request", "multipart field 'file' required", nil)
		return
	}
	if len(files) > maxFilesPerReq {
		files = files[:maxFilesPerReq]
	}

	dir := filepath.Join(s.dataDir, "uploads", rt, sess)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, 500, "internal", "mkdir uploads: "+err.Error(), nil)
		return
	}

	items := make([]core.Attachment, 0, len(files))
	for _, fh := range files {
		att, err := saveUpload(dir, fh)
		if err != nil {
			writeErr(w, 500, "internal", "save upload: "+err.Error(), nil)
			return
		}
		items = append(items, att)
	}
	writeJSON(w, 201, map[string]any{"items": items})
}

func saveUpload(dir string, fh *multipart.FileHeader) (core.Attachment, error) {
	src, err := fh.Open()
	if err != nil {
		return core.Attachment{}, err
	}
	defer src.Close()

	id := newUploadID()
	safe := sanitizeFilename(fh.Filename)
	dstPath := filepath.Join(dir, id+"-"+safe)
	dst, err := os.Create(dstPath)
	if err != nil {
		return core.Attachment{}, err
	}
	n, err := io.Copy(dst, io.LimitReader(src, uploadMaxBytes+1))
	cerr := dst.Close()
	if err != nil {
		return core.Attachment{}, err
	}
	if cerr != nil {
		return core.Attachment{}, cerr
	}
	if n > uploadMaxBytes {
		_ = os.Remove(dstPath)
		return core.Attachment{}, fmt.Errorf("file %q exceeds 50MB", fh.Filename)
	}
	return core.Attachment{ID: id, Name: safe, Path: dstPath, Size: n}, nil
}

func newUploadID() string {
	var b [8]byte
	rand.Read(b[:])
	return "att_" + hex.EncodeToString(b[:])
}

// sanitizeFilename：去路径成分、控制字符；空则回落 file。
func sanitizeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	name = strings.TrimSpace(b.String())
	name = strings.Trim(name, ".")
	if name == "" {
		return "file"
	}
	if len(name) > 128 {
		ext := filepath.Ext(name)
		name = name[:128-len(ext)] + ext
	}
	return name
}
