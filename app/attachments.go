package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxAttachment = 100 << 20

// Attachment is a file the user gave to an agent. The agent receives its path as a line of the message.
type Attachment struct {
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
	URL  string `json:"url"`
}

var (
	unsafeName     = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	attachmentLine = regexp.MustCompile(`^([0-9a-f]+)/([A-Za-z0-9_-][A-Za-z0-9._-]*)$`)
	inlineTypes    = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp", ".pdf": "application/pdf"}
)

func (s *Server) attachmentsDir() string {
	return filepath.Join(s.store.dir, "attachments")
}

func (s *Server) handleUpload(r *http.Request) (any, error) {
	// The name is reduced to one shell-safe word, so its path survives as a single token in a pasted message.
	name := strings.TrimLeft(unsafeName.ReplaceAllString(filepath.Base(r.URL.Query().Get("name")), "-"), ".")
	if name == "" {
		name = "file"
	}
	id := newID() + newID()
	dir := filepath.Join(s.attachmentsDir(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(f, http.MaxBytesReader(nil, r.Body, maxAttachment))
	f.Close()
	if err == nil && n == 0 {
		err = fail(400, "the file is empty")
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		err = fail(413, "the file is larger than %d MB", maxAttachment>>20)
	}
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return Attachment{Name: name, Path: path, URL: "/api/attachments/" + id + "/" + name}, nil
}

func (s *Server) handleAttachment(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("id") + "/" + r.PathValue("name")
	if !attachmentLine.MatchString(rel) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(s.attachmentsDir(), rel))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	head := make([]byte, 512)
	n, _ := f.Read(head)
	ext := strings.ToLower(filepath.Ext(rel))
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	// Uploaded files are served from the kk origin, so nothing in them may run as a page.
	switch {
	case inlineTypes[ext] != "":
		h.Set("Content-Type", inlineTypes[ext])
	case ext == ".svg":
		h.Set("Content-Type", "image/svg+xml")
		h.Set("Content-Security-Policy", "sandbox")
	case strings.HasPrefix(http.DetectContentType(head[:n]), "text/"):
		h.Set("Content-Type", "text/plain; charset=utf-8")
	default:
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", "attachment")
	}
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// withAttachments moves the attachment paths of a user message out of its text.
func (s *Server) withAttachments(e Event) Event {
	prefix := s.attachmentsDir() + "/"
	if e.Kind != kindUser || !strings.Contains(e.Text, prefix) {
		return e
	}
	var text []string
	for _, line := range strings.Split(e.Text, "\n") {
		rel, ok := strings.CutPrefix(strings.TrimSpace(line), prefix)
		if m := attachmentLine.FindStringSubmatch(rel); ok && m != nil {
			e.Attachments = append(e.Attachments, Attachment{Name: m[2], URL: "/api/attachments/" + rel})
		} else {
			text = append(text, line)
		}
	}
	e.Text = strings.TrimSpace(strings.Join(text, "\n"))
	return e
}
