package api

import (
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/weave-lab/interview-public/go/internal/auth"
	"github.com/weave-lab/interview-public/go/internal/store"
)

// Permit a 100 MiB file plus bounded multipart headers/form overhead.
const maxUploadSize = store.MaxFileSize + (1 << 20)

func (a *API) HandleUploadFile(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be multipart/form-data")
		return
	}
	if r.ContentLength > maxUploadSize {
		writeError(w, http.StatusRequestEntityTooLarge, "file too large")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	if err := r.ParseMultipartForm(1 << 20); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeError(w, http.StatusRequestEntityTooLarge, "file too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid multipart form")
		}
		return
	}
	// net/http also cleans up multipart files, but explicit cleanup keeps this
	// handler safe when reused directly and makes cleanup failures observable.
	defer func() {
		if err := r.MultipartForm.RemoveAll(); err != nil {
			log.Print("remove multipart temporary files failed")
		}
	}()
	if len(r.MultipartForm.Value) != 0 || len(r.MultipartForm.File) != 1 || len(r.MultipartForm.File["file"]) != 1 {
		writeError(w, http.StatusBadRequest, "expected exactly one file field")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing file field")
		return
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Print("close multipart request file failed")
		}
	}()
	if header.Size > store.MaxFileSize {
		writeError(w, http.StatusRequestEntityTooLarge, "file too large")
		return
	}
	if !validFilename(header.Filename) {
		writeError(w, http.StatusBadRequest, "invalid filename")
		return
	}

	f := &store.File{
		ID:          uuid.NewString(),
		Filename:    header.Filename,
		ContentType: safeContentType(header.Header.Get("Content-Type")),
	}

	if err := a.store.CreateFile(r.Context(), auth.UserID(r.Context()), f, file); err != nil {
		if errors.Is(err, store.ErrFileTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "file too large")
		} else if errors.Is(err, store.ErrFileExists) {
			writeError(w, http.StatusConflict, "file already exists")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to save file")
		}
		return
	}

	writeJSON(w, http.StatusCreated, f)
}

func (a *API) HandleDownloadFile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	file, meta, err := a.store.OpenFile(r.Context(), id)
	if errors.Is(err, store.ErrInvalidFileID) {
		writeError(w, http.StatusBadRequest, "invalid file ID")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to open file")
		return
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Print("close downloaded file failed")
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != meta.Size || meta.Size < 0 || meta.Size > store.MaxFileSize {
		writeError(w, http.StatusInternalServerError, "stored file is unavailable or inconsistent")
		return
	}
	if err := a.store.LogActivity(r.Context(), auth.UserID(r.Context()), "download_attempt", "file", id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record download attempt")
		return
	}

	filename := meta.Filename
	if !validFilename(filename) {
		filename = "download"
	}
	w.Header().Set("Content-Type", safeContentType(meta.ContentType))
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")

	if _, err := io.Copy(w, file); err != nil {
		log.Print("stream file response failed")
		panic(http.ErrAbortHandler)
	}
}

func (a *API) HandleListFiles(w http.ResponseWriter, r *http.Request) {
	files, err := a.store.ListFiles(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list files")
		return
	}

	writeJSON(w, http.StatusOK, files)
}

func validFilename(filename string) bool {
	if filename == "" || filename == "." || filename == ".." || len(filename) > 255 || strings.ContainsAny(filename, "/\\") {
		return false
	}
	for _, r := range filename {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func safeContentType(value string) string {
	if len(value) > 512 {
		return "application/octet-stream"
	}
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil || !strings.Contains(mediaType, "/") {
		return "application/octet-stream"
	}
	return mime.FormatMediaType(mediaType, params)
}
