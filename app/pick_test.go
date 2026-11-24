package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fake dialog stands in for osascript and zenity: it records its arguments and answers what the file "pick" says.
const fakeDialog = `#!/bin/sh
echo "$@" > "$FAKE_HOME/pick.args"
case "$(cat "$FAKE_HOME/pick")" in
cancel) echo "execution error: User canceled. (-128)" >&2; exit 1 ;;
broken) echo "execution error: no window server (-1700)" >&2; exit 2 ;;
*) cat "$FAKE_HOME/pick" ;;
esac
`

func TestPick(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"osascript", "zenity"} {
		os.WriteFile(filepath.Join(bin, name), []byte(fakeDialog), 0o755)
	}
	e := newEnv(t, "PATH="+bin+":"+fakeBin+":"+os.Getenv("PATH"), "DISPLAY=:0")
	chosen := filepath.Join(e.work, "chosen")
	os.Mkdir(chosen, 0o755)
	pick := func(answer, dir string, header ...string) (int, picked) {
		t.Helper()
		os.WriteFile(filepath.Join(e.fake, "pick"), []byte(answer), 0o644)
		os.Remove(filepath.Join(e.fake, "pick.args"))
		body, _ := json.Marshal(map[string]string{"dir": dir})
		req, _ := http.NewRequest("POST", "http://"+e.addr+"/api/pick", strings.NewReader(string(body)))
		req.AddCookie(&http.Cookie{Name: "kk_token", Value: e.token})
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		var p picked
		json.Unmarshal(out, &p)
		return resp.StatusCode, p
	}
	args := func() string {
		out, _ := os.ReadFile(filepath.Join(e.fake, "pick.args"))
		return string(out)
	}

	// macOS answers with a trailing slash.
	if code, p := pick(chosen+"/\n", e.work); code != 200 || !p.Native || p.Dir != chosen {
		t.Fatalf("pick a folder: %d %+v, want %s", code, p, chosen)
	}
	if !strings.Contains(args(), e.work) {
		t.Fatalf("the dialog starts in %q, want the directory in the field %s", args(), e.work)
	}
	home := filepath.Dir(e.work)
	if pick(chosen, filepath.Join(e.work, "missing")); !strings.Contains(args(), home) || strings.Contains(args(), "missing") {
		t.Fatalf("the dialog for a missing directory starts in %q, want the home directory %s", args(), home)
	}
	if code, p := pick("cancel", e.work); code != 200 || !p.Native || p.Dir != "" {
		t.Fatalf("cancel: %d %+v, want a native dialog and no directory", code, p)
	}
	if code, p := pick("broken", e.work); code != 200 || p.Native {
		t.Fatalf("a dialog that fails: %d %+v, want the in-page browser", code, p)
	}
	if code, p := pick(chosen, e.work, "X-Forwarded-For", "203.0.113.7"); code != 200 || p.Native || args() != "" {
		t.Fatalf("a browser on another machine: %d %+v, dialog ran with %q, want the in-page browser and no dialog", code, p, args())
	}
}
