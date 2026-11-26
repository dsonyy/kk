package main

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestImagePaths(t *testing.T) {
	text := "See /tmp/a.png, `/tmp/b.JPG` and **/tmp/c.webp**.\n```\n/tmp/d.gif\n```\n![x](/tmp/e.jpeg) [y](/tmp/f.png)\n" +
		"Not https://host/g.png, file:///tmp/h.png, src/i.png, /tmp/j.png.bak, /tmp/k.svg or /tmp/a.png twice."
	want := []string{"/tmp/a.png", "/tmp/b.JPG", "/tmp/c.webp", "/tmp/d.gif", "/tmp/e.jpeg", "/tmp/f.png"}
	if got := imagePaths(text); !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func writeImage(t *testing.T, path string, encode func(*bytes.Buffer, image.Image) error) []byte {
	t.Helper()
	var data bytes.Buffer
	if err := encode(&data, image.NewRGBA(image.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestLocalImages(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(t.TempDir(), "shots")
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	at := func(name string) string { return filepath.Join(dir, name) }
	shot := writeImage(t, at("shot.png"), func(b *bytes.Buffer, i image.Image) error { return png.Encode(b, i) })
	photo := writeImage(t, at("photo.jpg"), func(b *bytes.Buffer, i image.Image) error { return jpeg.Encode(b, i, nil) })
	writeImage(t, at("unnamed.png"), func(b *bytes.Buffer, i image.Image) error { return png.Encode(b, i) })
	os.WriteFile(at("text.png"), []byte("not a picture"), 0o644)
	os.WriteFile(at("drawing.svg"), []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), 0o644)
	os.Symlink(at("shot.png"), at("link.png"))
	os.Symlink(dir, at("linked-dir"))
	os.Symlink("/etc/passwd", at("passwd.png"))
	syscall.Mkfifo(at("fifo.png"), 0o644)

	refused := []string{
		at("unnamed.png"), at("text.png"), at("drawing.svg"), at("link.png"), at("linked-dir/shot.png"), at("passwd.png"),
		at("fifo.png"), at("gone.png"), dir + "/sub/../shot.png", "/etc/passwd",
	}
	named := []string{at("shot.png"), "`" + at("photo.jpg") + "`"}
	for _, p := range refused[1:] {
		named = append(named, p)
	}
	id := e.mustKK("new", "claude", "-C", e.work, "Pictures:\n\n"+strings.Join(named, "\n"))
	c := e.waitFor("answer", id, func(c convView) bool { return c.Status == statusIdle && hasEvent(c, kindAssistant, "echo: Pictures:\n\n"+strings.Join(named, "\n")) })
	var shown []Attachment
	for _, ev := range c.Events {
		if ev.Kind == kindAssistant {
			shown = ev.Images
		}
	}
	if len(shown) != 2 || shown[0].Path != at("shot.png") || shown[1].Path != at("photo.jpg") || shown[1].Name != "photo.jpg" {
		t.Fatalf("images of the answer: %+v", shown)
	}

	for i, want := range []struct {
		kind string
		data []byte
	}{{"image/png", shot}, {"image/jpeg", photo}} {
		resp, body := e.request("GET", shown[i].URL, nil)
		h := resp.Header
		if resp.StatusCode != 200 || !bytes.Equal(body, want.data) || h.Get("Content-Type") != want.kind || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Security-Policy") != "sandbox" {
			t.Fatalf("GET %s: %d %v", shown[i].URL, resp.StatusCode, h)
		}
	}
	image := func(id, path string) string { return "/api/conversations/" + id + "/image?path=" + url.QueryEscape(path) }
	other := e.mustKK("new", "claude", "-C", e.work, "nothing here")
	for _, p := range refused {
		if resp, body := e.request("GET", image(id, p), nil); resp.StatusCode != 404 {
			t.Errorf("GET %s: %d %q", p, resp.StatusCode, body)
		}
	}
	for _, path := range []string{image(other, at("shot.png")), image("nope", at("shot.png")), "/api/conversations/" + id + "/image"} {
		if resp, _ := e.request("GET", path, nil); resp.StatusCode != 404 {
			t.Errorf("GET %s: %d", path, resp.StatusCode)
		}
	}

	resp, err := http.Get("http://" + e.addr + shown[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("without the token: %d", resp.StatusCode)
	}

	e.mustKK("close", id)
	e.waitFor("closed", id, func(c convView) bool { return c.Closed })
	e.stop()
	e.start()
	if resp, _ := e.request("GET", shown[0].URL, nil); resp.StatusCode != 200 {
		t.Fatalf("a closed conversation after a restart: %d", resp.StatusCode)
	}
}
