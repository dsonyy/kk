package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
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

// openImage opens a local picture that is safe to show: a clean absolute path without user symlinks to a regular file whose
// content is a raster image. Its content type comes from the content, not the name.
func openImage(path string) (*os.File, string, error) {
	notImage := errors.New("not an image")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !imageExt.MatchString(path) {
		return nil, "", notImage
	}
	if userLinks(path) {
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

// userLinks reports whether a directory of path is a symlink that is not part of the system layout, which root owns, as
// /tmp and /var on macOS. A server run as root cannot tell the two apart.
func userLinks(path string) bool {
	for dir := filepath.Dir(path); dir != "/"; dir = filepath.Dir(dir) {
		st, err := os.Lstat(dir)
		if err != nil {
			return true
		}
		if st.Mode()&os.ModeSymlink != 0 && (st.Sys().(*syscall.Stat_t).Uid != 0 || os.Geteuid() == 0) {
			return true
		}
	}
	return false
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

var pastePlaceholder = regexp.MustCompile(`\[Image #(\d+)\][ \t]*`)

// shown is a message as the page shows it: with its attachments, the pictures it names and the images pasted into it.
func (s *Server) shown(id string, e Event) Event {
	return withPasted(id, withImages(id, s.withAttachments(e)))
}

// withPasted lists the images pasted into a Claude Code prompt as the message's attachments, with the address that serves
// each, and drops their placeholders from the text.
func withPasted(id string, e Event) Event {
	if len(e.pasted) == 0 {
		return e
	}
	shown := map[string]bool{}
	for _, p := range e.pasted {
		name := "Image"
		if p.n > 0 {
			name = fmt.Sprintf("Image #%d", p.n)
			shown[strconv.Itoa(p.n)] = true
		}
		e.Attachments = append(e.Attachments, Attachment{Name: name, URL: fmt.Sprintf("/api/conversations/%s/pasted/%s/%d", id, e.ID, p.block), Image: true})
	}
	e.Text = strings.TrimSpace(pastePlaceholder.ReplaceAllStringFunc(e.Text, func(m string) string {
		if shown[pastePlaceholder.FindStringSubmatch(m)[1]] {
			return ""
		}
		return m
	}))
	return e
}

// handlePasted serves an image pasted into a prompt of the conversation. It reads the image from the transcript line the
// message came from, so the server keeps no image in memory.
func (s *Server) handlePasted(w http.ResponseWriter, r *http.Request) {
	c, ok := s.store.get(r.PathValue("id"))
	block, err := strconv.Atoi(r.PathValue("block"))
	if !ok || err != nil {
		http.NotFound(w, r)
		return
	}
	if c.Closed {
		s.readTail(c)
	}
	uuid, path, line := r.PathValue("uuid"), "", int64(-1)
	s.mu.Lock()
	if t := s.tails[c.ID]; t != nil && t.path == c.Transcript {
		path = t.path
		for _, e := range t.Events {
			if e.ID == uuid && slices.ContainsFunc(e.pasted, func(p pastedImage) bool { return p.block == block }) {
				line = e.line
			}
		}
	}
	s.mu.Unlock()
	data, kind := pastedData(path, line, uuid, block)
	if data == nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", kind)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

// pastedData decodes the image block of the transcript line at offset line, when that line is still the message uuid.
func pastedData(path string, line int64, uuid string, block int) ([]byte, string) {
	if line < 0 {
		return nil, ""
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ""
	}
	defer f.Close()
	raw, err := bufio.NewReader(io.NewSectionReader(f, line, math.MaxInt64-line)).ReadBytes('\n')
	if err != nil {
		return nil, ""
	}
	var l struct {
		UUID    string `json:"uuid"`
		Message struct {
			Content []struct {
				Type   string `json:"type"`
				Source struct {
					Type string `json:"type"`
					Data string `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &l) != nil || l.UUID != uuid || block >= len(l.Message.Content) {
		return nil, ""
	}
	b := l.Message.Content[block]
	data, err := base64.StdEncoding.DecodeString(b.Source.Data)
	if b.Type != "image" || b.Source.Type != "base64" || err != nil {
		return nil, ""
	}
	if kind := http.DetectContentType(data); slices.Contains(imageTypes, kind) {
		return data, kind
	}
	return nil, ""
}
