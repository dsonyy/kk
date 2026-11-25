package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

// handleTerminal attaches a browser terminal to the conversation's tmux window, without the status bar.
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	c, ok := s.store.get(r.PathValue("id"))
	if !ok || c.Window == "" {
		http.Error(w, "no window", http.StatusNotFound)
		return
	}
	attach(w, r, "window "+c.Window, "set-option", "status", "off", ";", "select-window", "-t", c.Window)
}

// handleTmux attaches a browser terminal to the whole kk session, with the status bar and the mouse to switch windows.
// The status bar names the session kk instead of the viewer's own session.
func (s *Server) handleTmux(w http.ResponseWriter, r *http.Request) {
	if err := ensureSession(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	attach(w, r, "the session", "set-option", "status", "on", ";", "set-option", "status-left", "[kk] ", ";", "set-option", "mouse", "on")
}

// attach runs a tmux client for a browser terminal and then the tmux commands in setup. Each viewer gets its own
// session grouped with kk's, so it can switch windows without switching what other tmux clients see.
func attach(w http.ResponseWriter, r *http.Request, what string, setup ...string) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	b := make([]byte, 6)
	rand.Read(b)
	name := fmt.Sprintf("kk-view-%x", b)
	log.Printf("terminal %s attached to %s", name, what)
	defer log.Printf("terminal %s detached", name)
	cmd := tmuxCmd(append([]string{"new-session", "-t", "=" + tmuxSession, "-s", name, ";", "set-option", "destroy-unattached", "on", ";"}, setup...)...)
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(v string) bool { return strings.HasPrefix(v, "TMUX=") }), "TERM=xterm-256color")
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 120, Rows: 40})
	if err != nil {
		ws.Close(websocket.StatusInternalError, err.Error())
		return
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	defer tty.Close()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		defer cancel()
		buf := make([]byte, 32<<10)
		for {
			n, err := tty.Read(buf)
			if err != nil || ws.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
				return
			}
		}
	}()
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			tty.Write(data)
			continue
		}
		var size struct{ Cols, Rows uint16 }
		if json.Unmarshal(data, &size) == nil && size.Cols > 0 && size.Rows > 0 {
			pty.Setsize(tty, &pty.Winsize{Cols: size.Cols, Rows: size.Rows})
		}
	}
}
