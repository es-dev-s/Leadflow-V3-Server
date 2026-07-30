package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

const (
	maxProofUploadBytes = 5 << 20 // 5 MiB
	firstResponseSubdir = "first-response"
)

var allowedProofMIME = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
}

type UploadStore struct {
	root string
}

func NewUploadStore(root string) (*UploadStore, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "uploads"
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, firstResponseSubdir), 0o755); err != nil {
		return nil, err
	}
	return &UploadStore{root: abs}, nil
}

func (u *UploadStore) firstResponseDir() string {
	return filepath.Join(u.root, firstResponseSubdir)
}

func (u *UploadStore) resolveFirstResponse(name string) (string, error) {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("invalid file")
	}
	full := filepath.Join(u.firstResponseDir(), name)
	if !strings.HasPrefix(full, u.firstResponseDir()+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid file")
	}
	return full, nil
}

func proofPublicPath(filename string) string {
	return "/api/uploads/first-response/" + filename
}

func proofStoredNameFromPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "/api/uploads/first-response/") {
		return filepath.Base(path)
	}
	return filepath.Base(path)
}

func (s *Server) handleUploadFirstResponseProof(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !canCreateLeads(authUser.Role) && !canEditLeadProfile(authUser.Role) {
		writeError(w, http.StatusForbidden, "you do not have permission to upload proof")
		return
	}
	if s.uploads == nil {
		writeError(w, http.StatusInternalServerError, "upload storage is not available")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxProofUploadBytes+64<<10)
	if err := r.ParseMultipartForm(maxProofUploadBytes + 64<<10); err != nil {
		writeError(w, http.StatusBadRequest, "file too large (max 5 MB)")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "file is required")
		return
	}
	defer file.Close()

	if header.Size > maxProofUploadBytes {
		writeError(w, http.StatusBadRequest, "file too large (max 5 MB)")
		return
	}

	sniff := make([]byte, 512)
	n, err := file.Read(sniff)
	if err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "failed to read file")
		return
	}
	mime := http.DetectContentType(sniff[:n])
	ext, ok := allowedProofMIME[mime]
	if !ok {
		writeError(w, http.StatusBadRequest, "only JPEG, PNG, or WebP images are allowed")
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to process file")
		return
	}

	filename := uuid.NewString() + ext
	destPath := filepath.Join(s.uploads.firstResponseDir(), filename)
	dest, err := os.OpenFile(destPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("create proof file: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to store file")
		return
	}
	defer dest.Close()

	written, err := io.Copy(dest, io.LimitReader(file, maxProofUploadBytes+1))
	if err != nil {
		_ = os.Remove(destPath)
		log.Printf("write proof file: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to store file")
		return
	}
	if written > maxProofUploadBytes {
		_ = os.Remove(destPath)
		writeError(w, http.StatusBadRequest, "file too large (max 5 MB)")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"path":     proofPublicPath(filename),
		"filename": filename,
		"mime":     mime,
		"size":     written,
	})
}

func (s *Server) handleServeFirstResponseProof(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireLeadDataAccess(w, r) {
		return
	}
	if s.uploads == nil {
		writeError(w, http.StatusInternalServerError, "upload storage is not available")
		return
	}

	name := strings.TrimPrefix(r.URL.Path, "/api/uploads/first-response/")
	name = strings.Trim(name, "/")
	if name == "" || strings.Contains(name, "/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	full, err := s.uploads.resolveFirstResponse(name)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	f, err := os.Open(full)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil || stat.IsDir() {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	ext := strings.ToLower(filepath.Ext(full))
	switch ext {
	case ".jpg", ".jpeg":
		w.Header().Set("Content-Type", "image/jpeg")
	case ".png":
		w.Header().Set("Content-Type", "image/png")
	case ".webp":
		w.Header().Set("Content-Type", "image/webp")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, stat.Name(), stat.ModTime(), f)
}
