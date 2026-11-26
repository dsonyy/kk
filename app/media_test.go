package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMediaView(t *testing.T) {
	e := newEnv(t)
	// The server shows no picture behind a symlink, and the temporary directory of macOS is one.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	picture := func(name string, w, h int, encode func(*bytes.Buffer, image.Image) error) string {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{200, 60, 60, 255}), image.Point{}, draw.Src)
		var data bytes.Buffer
		if err := encode(&data, img); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		os.WriteFile(path, data.Bytes(), 0o644)
		return path
	}
	toPNG := func(b *bytes.Buffer, i image.Image) error { return png.Encode(b, i) }
	a := picture("a.png", 640, 400, toPNG)
	b := picture("b.png", 320, 200, toPNG)
	c := picture("c.jpg", 300, 120, func(b *bytes.Buffer, i image.Image) error { return jpeg.Encode(b, i, nil) })
	var data bytes.Buffer
	png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 50, 50)))
	attached := e.upload("attached.png", data.Bytes())
	notes := e.upload("notes.txt", []byte("not a picture"))

	// The fake agent answers with the prompt, so every path is named twice per turn, and a is named in both turns.
	id := e.mustKK("new", "claude", "-C", e.work, fmt.Sprintf("Look at %s and %s.\n\n%s\n%s", a, b, attached.Path, notes.Path))
	e.waitFor("answer", id, func(c convView) bool { return c.Status == statusIdle && len(c.Events) >= 2 })
	e.mustKK("send", id, fmt.Sprintf("Now %s and %s.", c, a))
	e.waitFor("second answer", id, func(c convView) bool { return c.Status == statusIdle && len(c.Events) >= 4 })
	empty := e.mustKK("new", "claude", "-C", e.work, "Nothing to see")
	e.waitFor("answer", empty, func(c convView) bool { return c.Status == statusIdle && len(c.Events) >= 2 })

	p := e.openPage(1400, 900)
	p.call("Browser.grantPermissions", map[string]any{"origin": e.url, "permissions": []string{"clipboardReadWrite", "clipboardSanitizedWrite"}})
	p.call("Emulation.setFocusEmulationEnabled", map[string]any{"enabled": true})
	p.eval(fmt.Sprintf(`location.hash = %q`, "~"+id), nil)
	panel := fmt.Sprintf(`document.getElementById("panel-%s")`, id)
	mode := func(label string) string {
		return fmt.Sprintf(`%s.querySelector('.modes button[aria-label="%s"]')`, panel, label)
	}
	type state struct {
		Modes, On []string
		Count     string
		Feed      bool
		Terminal  bool
		Media     bool
		Composer  bool
	}
	read := func() (s state) {
		t.Helper()
		p.eval(`(() => { const el = `+panel+`, modes = [...el.querySelectorAll(".modes button")]
			return { modes: modes.map((b) => b.ariaLabel), on: modes.filter((b) => b.classList.contains("on")).map((b) => b.ariaLabel),
				count: modes.find((b) => b.ariaLabel === "Media")?.dataset.count ?? "",
				feed: !el.querySelector(".feed").hidden, terminal: !!el.querySelector(".terminal"), media: !!el.querySelector(".media"), composer: !!el.querySelector(".composer") } })()`, &s)
		return s
	}

	p.until(mode("Media"))
	if s := read(); !slices.Equal(s.Modes, []string{"Chat", "Terminal", "Media"}) || !slices.Equal(s.On, []string{"Chat"}) || s.Count != "4" || !s.Feed || s.Media {
		t.Fatalf("before the switch: %+v", s)
	}
	p.tap(mode("Terminal"))
	p.until(panel + `.querySelector(".terminal")`)
	if s := read(); !slices.Equal(s.On, []string{"Terminal"}) || s.Feed || s.Media || s.Composer {
		t.Fatalf("terminal: %+v", s)
	}
	p.tap(mode("Media"))
	p.until(panel + `.querySelector(".media figure img")`)
	if s := read(); !slices.Equal(s.On, []string{"Media"}) || s.Feed || s.Terminal || s.Composer {
		t.Fatalf("media: %+v", s)
	}

	// Newest first, each file once with the time of the last message that names it; the text file is no picture.
	var tiles []struct{ Name, Path, Time, Src string }
	p.until(`[...document.querySelectorAll(".media img")].every((i) => i.naturalWidth)`)
	p.eval(`[...document.querySelectorAll(".media figure")].map((f) => ({ name: f.querySelector(".name").textContent,
		path: f.querySelector("a").title, time: f.querySelector("time").dateTime, src: f.querySelector("img").getAttribute("src") }))`, &tiles)
	var paths []string
	for _, tile := range tiles {
		paths = append(paths, tile.Path)
	}
	if want := []string{c, a, b, attached.Path}; !slices.Equal(paths, want) {
		t.Fatalf("tiles %q, want %q", paths, want)
	}
	var last string
	for _, ev := range e.conv(id).Events {
		if strings.Contains(ev.Text, a) {
			last = ev.Time.Format(time.RFC3339Nano)
		}
	}
	if tiles[0].Name != "c.jpg" || tiles[1].Time != last || tiles[3].Src != attached.URL {
		t.Fatalf("tiles: %+v, want a at %s", tiles, last)
	}

	tile := func(i int, what string) string {
		return fmt.Sprintf(`document.querySelectorAll(".media figure")[%d].querySelector('%s')`, i, what)
	}
	caption := func() string {
		var s string
		p.eval(`document.querySelector("dialog.viewer[open] .caption")?.textContent ?? ""`, &s)
		return s
	}
	p.tap(tile(1, `button[aria-label="Open"]`))
	p.until(`document.querySelector("dialog.viewer[open] img")?.naturalWidth`)
	if got := caption(); got != a {
		t.Fatalf("Open shows %q, want %q", got, a)
	}
	p.press("ArrowRight", 39, "")
	if got := caption(); got != b {
		t.Fatalf("the next picture is %q, want %q", got, b)
	}
	p.press("Escape", 27, "")
	p.until(`!document.querySelector("dialog.viewer")`)
	var focus bool
	p.eval(`document.activeElement === `+tile(1, `button[aria-label="Open"]`), &focus)
	if !focus {
		t.Fatal("the focus did not go back to Open")
	}
	// The thumbnail opens the viewer from the keyboard too.
	p.eval(tile(2, "a")+".focus()", nil)
	p.press("Enter", 13, "\r")
	p.until(`document.querySelector("dialog.viewer[open]")`)
	if got := caption(); got != b {
		t.Fatalf("Enter on a thumbnail shows %q, want %q", got, b)
	}
	p.press("Escape", 27, "")
	p.until(`!document.querySelector("dialog.viewer")`)

	clipboard := func(read string) string {
		t.Helper()
		p.eval(`window.clip = undefined; `+read+`.then((v) => (window.clip = v), (e) => (window.clip = "error: " + e.message))`, nil)
		p.until(`window.clip !== undefined`)
		var s string
		p.eval(`String(window.clip)`, &s)
		return s
	}
	flashed := func(want string) {
		t.Helper()
		p.until(fmt.Sprintf(`document.getElementById("flash")?.textContent === %q`, want))
		p.eval(`document.getElementById("flash").remove()`, nil)
	}
	// The mnemonic of a tile's button acts on the tile that has the focus.
	p.eval(tile(2, `button[aria-label="Open"]`)+".focus()", nil)
	p.press("p", 80, "p")
	flashed("Path copied.")
	if got := clipboard(`navigator.clipboard.readText()`); got != b {
		t.Fatalf("Copy path copied %q, want %q", got, b)
	}
	p.tap(tile(0, `button[aria-label="Copy path"]`))
	flashed("Path copied.")
	if got := clipboard(`navigator.clipboard.readText()`); got != c {
		t.Fatalf("Copy path copied %q, want %q", got, c)
	}

	// The JPEG reaches the clipboard as a PNG of the same size.
	bitmap := `navigator.clipboard.read().then(async ([item]) => { const blob = await item.getType("image/png"), b = await createImageBitmap(blob); return item.types.join(",") + " " + b.width + "x" + b.height })`
	p.tap(tile(0, `button[aria-label="Copy image"]`))
	flashed("Image copied.")
	if got := clipboard(bitmap); got != "image/png 300x120" {
		t.Fatalf("Copy image of the JPEG: %q", got)
	}
	p.tap(tile(1, `button[aria-label="Copy image"]`))
	flashed("Image copied.")
	if got := clipboard(bitmap); got != "image/png 640x400" {
		t.Fatalf("Copy image of the PNG: %q", got)
	}

	p.eval(fmt.Sprintf(`location.hash = %q`, "~"+empty), nil)
	emptyPanel := fmt.Sprintf(`document.getElementById("panel-%s")`, empty)
	p.tap(emptyPanel + `.querySelector('.modes button[aria-label="Media"]')`)
	p.until(emptyPanel + `.querySelector(".media")`)
	var none struct{ Text, Count string }
	p.eval(`(() => { const el = `+emptyPanel+`; return { text: el.querySelector(".media").textContent, count: el.querySelector('.modes button[aria-label="Media"]').dataset.count ?? "" } })()`, &none)
	if none.Text != "No images in this conversation." || none.Count != "" {
		t.Fatalf("empty: %+v", none)
	}

	// A closed session has no terminal, and its pictures stay.
	if status, out := e.post("/api/conversations/"+id+"/close", map[string]any{}); status != 200 {
		t.Fatalf("close: %d %s", status, out)
	}
	p.eval(fmt.Sprintf(`location.hash = %q`, "~"+id), nil)
	p.until(fmt.Sprintf(`%s?.querySelectorAll(".modes button").length === 2`, panel))
	p.tap(mode("Media"))
	p.until(panel + `.querySelectorAll(".media figure").length === 4`)
	if s := read(); !slices.Equal(s.Modes, []string{"Chat", "Media"}) || !strings.Contains(strings.Join(s.On, ","), "Media") {
		t.Fatalf("closed: %+v", s)
	}
}
