package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Claude Code keeps an image pasted into its prompt in the transcript, as an image block after the text that holds its
// [Image #N] placeholder.
func TestPastedImages(t *testing.T) {
	e := newCoordinatorEnv(t)
	for n, size := range map[int]image.Point{96: {1600, 900}, 97: {800, 1200}, 98: {2400, 1600}} {
		img := image.NewRGBA(image.Rectangle{Max: size})
		draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{uint8(n), 90, 140, 255}), image.Point{}, draw.Src)
		var data bytes.Buffer
		png.Encode(&data, img)
		os.WriteFile(filepath.Join(e.fake, fmt.Sprintf("paste-%d.png", n)), data.Bytes(), 0o644)
	}
	alpha := filepath.Join(e.work, "alpha")
	os.MkdirAll(alpha, 0o755)
	id := e.mustKK("new", "claude", "-C", alpha, "[Image #98]")
	e.waitFor("answer", id, turnEnded)
	e.mustKK("send", id, `[Image #96] why is it still "working". [Image #97] - i hard refreshed`)
	e.waitFor("second answer", id, func(c convView) bool { return turnEnded(c) && len(c.Events) >= 6 })

	// The events carry addresses, never the pictures.
	if _, body := e.request("GET", "/api/conversations/"+id, nil); bytes.Contains(body, []byte("iVBORw0KGgo")) {
		t.Fatal("the conversation holds the pictures themselves")
	}
	var urls []string
	for _, ev := range e.conv(id).Events {
		if ev.Kind == kindUser {
			for _, a := range ev.Attachments {
				urls = append(urls, a.URL)
			}
		}
	}
	if len(urls) != 3 {
		t.Fatalf("pasted images: %q", urls)
	}
	for _, u := range urls {
		resp, body := e.request("GET", u, nil)
		if cfg, err := png.DecodeConfig(bytes.NewReader(body)); resp.StatusCode != 200 || err != nil || resp.Header.Get("Content-Type") != "image/png" || cfg.Width == 0 {
			t.Fatalf("%s: %d %s %v", u, resp.StatusCode, resp.Header.Get("Content-Type"), err)
		}
	}
	// Only an image block of a message of this conversation is served.
	other := e.mustKK("new", "claude", "-C", e.work, "hello")
	e.waitFor("answer", other, turnEnded)
	uuid, block, _ := strings.Cut(strings.TrimPrefix(urls[0], "/api/conversations/"+id+"/pasted/"), "/")
	for _, u := range []string{
		"/api/conversations/" + other + "/pasted/" + uuid + "/" + block,
		"/api/conversations/" + id + "/pasted/" + uuid + "/0",
		"/api/conversations/" + id + "/pasted/" + uuid + "/9",
		"/api/conversations/" + id + "/pasted/nonsense/1",
	} {
		if resp, _ := e.request("GET", u, nil); resp.StatusCode != 404 {
			t.Errorf("%s: %d, want 404", u, resp.StatusCode)
		}
	}

	p := e.openPage(1400, 900)
	p.openSession(id)
	panel := fmt.Sprintf(`document.getElementById("panel-%s")`, id)
	p.until(panel + `?.querySelectorAll(".msg.user .attachments img").length === 3 && [...` + panel + `.querySelectorAll(".msg.user img")].every((i) => i.naturalWidth)`)
	type message struct {
		Text   string
		Widths []int
	}
	var got []message
	p.eval(`[...`+panel+`.querySelectorAll(".feed .msg.user")].map((m) => ({
		text: m.querySelector(".md")?.textContent.trim() ?? "",
		widths: [...m.querySelectorAll(".attachments img")].map((i) => i.naturalWidth),
	}))`, &got)
	want := []message{{"", []int{2400}}, {`why is it still "working". - i hard refreshed`, []int{1600, 800}}}
	if len(got) != 2 || got[0].Text != want[0].Text || !slices.Equal(got[0].Widths, want[0].Widths) || got[1].Text != want[1].Text || !slices.Equal(got[1].Widths, want[1].Widths) {
		t.Fatalf("user messages: %+v, want %+v", got, want)
	}
	p.shot("pasted-chat")

	// A click opens the picture large, and the viewer steps through the pictures of the message.
	p.tap(panel + `.querySelectorAll(".feed .msg.user")[1].querySelector(".attachments img")`)
	p.until(`document.querySelector("dialog.viewer[open] img")?.naturalWidth === 1600`)
	p.shot("pasted-viewer")
	p.press("ArrowRight", 39, "")
	p.until(`document.querySelector("dialog.viewer[open] img")?.naturalWidth === 800`)
	p.press("Escape", 27, "")
	p.until(`!document.querySelector("dialog.viewer")`)

	p.click(panel + `.querySelector('.modes button[aria-label="Media"]')`)
	p.until(panel + `?.querySelectorAll(".media figure img").length === 3 && [...` + panel + `.querySelectorAll(".media img")].every((i) => i.naturalWidth)`)
	var media struct {
		Count string
		Names []string
		Paths int
	}
	p.eval(`({ count: `+panel+`.querySelector('.modes button[aria-label="Media"]').dataset.count,
		names: [...`+panel+`.querySelectorAll(".media figcaption .name")].map((n) => n.textContent),
		paths: `+panel+`.querySelectorAll('.media button[aria-label="Copy path"]').length })`, &media)
	if media.Count != "3" || !slices.Equal(media.Names, []string{"Image #96", "Image #97", "Image #98"}) || media.Paths != 0 {
		t.Fatalf("media: %+v", media)
	}
	p.shot("pasted-media")

	// The konduktor's panel shows them the same way.
	e.startKonduktor(alpha, "[Image #98]")
	p.dirTab("alpha")
	p.until(`document.querySelector(".side-panel .msg.user .attachments img")?.naturalWidth === 2400`)
	if text := p.texts(".side-panel .msg.user"); !slices.Equal(text, []string{""}) {
		t.Fatalf("konduktor's user messages: %q", text)
	}
	p.shot("pasted-konduktor")
}
