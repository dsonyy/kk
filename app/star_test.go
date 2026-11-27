package main

import (
	"strings"
	"testing"
)

func TestStar(t *testing.T) {
	e := newEnv(t)
	id := e.mustKK("new", "claude", "-C", e.work, "hello")
	e.waitFor("answer", id, turnEnded)
	queued := e.mustKK("new", "claude", "-C", e.work, "-after", id, "Next step")

	e.mustKK("star", id)
	e.mustKK("star", queued)
	e.mustKK("star", queued)
	if a, b := e.conv(id), e.conv(queued); !a.Starred || !b.Starred {
		t.Fatalf("starred: %v and %v", a.Starred, b.Starred)
	}
	if ls := e.mustKK("ls"); !strings.Contains(ls, "★ "+e.conv(id).Title) || !strings.Contains(ls, "★ Next step") {
		t.Fatalf("kk ls:\n%s", ls)
	}
	if out, err := e.kk("star", "nope"); err == nil {
		t.Fatalf("star of a missing conversation: %s", out)
	}

	e.mustKK("unstar", queued)
	e.stop()
	e.start()
	if a, b := e.conv(id), e.conv(queued); !a.Starred || b.Starred {
		t.Fatalf("after restart: %v and %v", a.Starred, b.Starred)
	}
	if ls := e.mustKK("ls"); strings.Contains(ls, "★ Next step") {
		t.Fatalf("kk ls after unstar:\n%s", ls)
	}

	if resp, body := e.request("DELETE", "/api/conversations/"+id+"/star", nil); resp.StatusCode != 200 || strings.Contains(string(body), `"starred"`) {
		t.Fatalf("unstar over the API: %d %s", resp.StatusCode, body)
	}
}
