package main

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
)

var (
	pathToken  = regexp.MustCompile("/[^\\s\"'<>()\\[\\]`]+")
	imageExt   = regexp.MustCompile(`(?i)\.(png|jpe?g|gif|webp)$`)
	imageTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}
)

// imagePaths returns the absolute image paths a message names: alone, in code, in markdown image or link syntax. A path
// glued to a word or a scheme, as in a URL, is not one.
func imagePaths(text string) []string {
	var out []string
	for _, m := range pathToken.FindAllStringIndex(text, -1) {
		if m[0] > 0 && !strings.ContainsRune(" \t\n\"'(<[`*_", rune(text[m[0]-1])) {
			continue
		}
		p := strings.TrimRight(text[m[0]:m[1]], ".,;:!?*_")
		if imageExt.MatchString(p) && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// openImage opens a local picture that is safe to show: a clean absolute path without symlinks to a regular file whose
// content is a raster image. Its content type comes from the content, not the name.
func openImage(path string) (*os.File, string, error) {
	notImage := errors.New("not an image")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !imageExt.MatchString(path) {
		return nil, "", notImage
	}
	if real, err := filepath.EvalSymlinks(path); err != nil || real != path {
		return nil, "", notImage
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, "", notImage
	}
	head := make([]byte, 512)
	n, _ := f.Read(head)
	kind := http.DetectContentType(head[:n])
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || !slices.Contains(imageTypes, kind) {
		f.Close()
		return nil, "", notImage
	}
	return f, kind, nil
}

// withImages lists the pictures a message names that can be shown, with the address that serves them.
func withImages(id string, e Event) Event {
	if e.Kind != kindUser && e.Kind != kindAssistant {
		return e
	}
	for _, p := range imagePaths(e.Text) {
		if f, _, err := openImage(p); err == nil {
			f.Close()
			e.Images = append(e.Images, Attachment{Name: filepath.Base(p), Path: p, URL: "/api/conversations/" + id + "/image?path=" + url.QueryEscape(p)})
		}
	}
	return e
}

// handleImage serves a picture that a message of the conversation names, so the server reads no other file.
func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	c, ok := s.store.get(r.PathValue("id"))
	if !ok || !s.names(c, path) {
		http.NotFound(w, r)
		return
	}
	f, kind, err := openImage(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	h := w.Header()
	h.Set("Content-Type", kind)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// names reports whether a message of c's transcript names the image path.
func (s *Server) names(c Conversation, path string) bool {
	if c.Closed {
		// The tick reads the transcripts of open conversations and acts on what is new.
		s.readTail(c)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tails[c.ID]
	if t == nil || t.path != c.Transcript {
		return false
	}
	for _, e := range t.Events {
		if (e.Kind == kindUser || e.Kind == kindAssistant) && slices.Contains(imagePaths(e.Text), path) {
			return true
		}
	}
	return false
}
