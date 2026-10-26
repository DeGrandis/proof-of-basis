package app

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

// Files kept as proof for a record: receipts, a charity's acknowledgment,
// a gift letter, an exchange's statement. They go into backups and the
// accountant package.

const maxAttachment = 10 << 20

// safeTypes are served with their own type; anything else downloads as
// bytes, so an uploaded page can never run as part of this app.
var safeTypes = map[string]bool{"application/pdf": true, "image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "text/plain": true, "text/csv": true}

func (s *Server) listAttachments(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.Attachments(r.URL.Query().Get("key"))
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, list)
}

func (s *Server) postAttachment(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAttachment+1<<20)
	if err := r.ParseMultipartForm(maxAttachment + 1<<20); err != nil {
		fail(w, 400, errors.New("files can be up to 10 MB"))
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if key == "" {
		fail(w, 400, errors.New("which record is this for?"))
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		fail(w, 400, errors.New("choose a file"))
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxAttachment+1))
	if err != nil {
		fail(w, 400, err)
		return
	}
	if len(data) > maxAttachment {
		fail(w, 400, errors.New("files can be up to 10 MB"))
		return
	}
	name := filepath.Base(strings.ReplaceAll(hdr.Filename, "\\", "/"))
	typ := http.DetectContentType(data)
	if t, _, err := mime.ParseMediaType(typ); err == nil {
		typ = t
	}
	a, err := s.st.AddAttachment(key, name, typ, clip(r.FormValue("note"), 300), data)
	if err != nil {
		fail(w, 500, err)
		return
	}
	s.note("attach", "Attached "+name+" to a record")
	writeJSON(w, a)
}

func (s *Server) getAttachment(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a, data, err := s.st.AttachmentData(id)
	if err != nil {
		fail(w, 404, errors.New("no such file"))
		return
	}
	typ := "application/octet-stream"
	if safeTypes[a.Mime] {
		typ = a.Mime
	}
	w.Header().Set("Content-Type", typ)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", a.Filename))
	w.Write(data)
}

func (s *Server) deleteAttachment(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.st.DeleteAttachment(id); err != nil {
		fail(w, 500, err)
		return
	}
	s.note("attach", "Removed an attached file")
	writeJSON(w, map[string]bool{"ok": true})
}

// getRecordSource shows an exchange record as the exchange's file had it:
// the proof behind it.
func (s *Server) getRecordSource(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	id, err := strconv.ParseInt(strings.TrimPrefix(key, "ex:"), 10, 64)
	if !strings.HasPrefix(key, "ex:") || err != nil {
		fail(w, 400, errors.New("only imported records have a source row"))
		return
	}
	src, err := s.st.EventSourceRow(id)
	if err != nil {
		fail(w, 404, errors.New("no such record"))
		return
	}
	writeJSON(w, src)
}
