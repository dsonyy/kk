package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const usage = `kk - web conductor for coding agents in tmux

  kk                          start the server
  kk url                      print the web address
  kk ls                       list conversations
  kk show <id> [-n N]         show the last N events of a conversation
  kk new <harness> [-m model] [-C dir] [-w] [-g group] <prompt>
                              start an agent; -w gives it its own git worktree and branch,
                              -g puts it in a feature group instead of letting kk decide
  kk group                    list the feature groups of every project
  kk group <id> <name>        move a conversation to a feature group
  kk send <id> <text|->       type a message into the agent
  kk screen <id>              print the agent's tmux screen
  kk keys <id> <key>...       press keys in the agent's window
  kk interrupt <id>           stop the agent's current turn
  kk stop <id>                end the agent process, resume restarts it
  kk reset <id>               start the conversation over with a clean context
  kk resume <id>              restart an exited agent
  kk close <id>               stop the agent and close the conversation, resume reopens it
  kk escalate <id> <reason>   hand a waiting agent from the conductor to the user, with a one-line reason
  kk hook <event>             hook endpoint for agents (payload on stdin)
`

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		if err := serve(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := runCLI(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "kk:", err)
		os.Exit(1)
	}
}

func sockPath() string { return filepath.Join(home(), "kk.sock") }

func addr() string { return loadSettings(home()).addr() }

func readToken(dir string) (string, error) {
	path := filepath.Join(dir, "token")
	if data, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(data)) > 0 {
		return string(bytes.TrimSpace(data)), nil
	}
	b := make([]byte, 16)
	rand.Read(b)
	t := hex.EncodeToString(b)
	return t, os.WriteFile(path, []byte(t+"\n"), 0o600)
}

func envDuration(name string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(name)); err == nil {
		return d
	}
	return def
}

func serve() error {
	if conn, err := net.Dial("unix", sockPath()); err == nil {
		conn.Close()
		return errors.New("kk is already running, open the address from `kk url`")
	}
	store, err := openStore(home())
	if err != nil {
		return err
	}
	if err := openLog(store.dir); err != nil {
		return err
	}
	token, err := readToken(store.dir)
	if err != nil {
		return err
	}
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	s := &Server{
		store: store, bin: bin, token: token, settings: loadSettings(store.dir),
		tails: map[string]*Tail{}, subs: map[chan string]bool{}, grouping: make(chan string, 4096), asked: map[string]int{},
		controls: map[string]time.Time{}, raises: map[string]int{}, watches: map[string]*watch{}, notices: map[string]notice{}, handovers: map[string]bool{},
	}
	os.Remove(sockPath())
	unixLn, err := net.Listen("unix", sockPath())
	if err != nil {
		return err
	}
	os.Chmod(sockPath(), 0o600)
	tcpLn, err := net.Listen("tcp", addr())
	if err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	s.inbox = openInbox(store.dir, func() map[string]InboxSettings { return s.cfg().Inbox })
	mux := s.routes()
	go watchStats()
	go s.groupLoop()
	s.backfillGroups()
	go http.Serve(unixLn, mux)
	go http.Serve(tcpLn, s.guard(mux))
	s.tick()
	if s.cfg().coordinator() {
		if err := s.ensureCoordinator(); err != nil {
			log.Printf("conductor: %v", err)
		}
	}
	log.Printf("kk started on %s", addr())
	// Only stdout gets the token.
	fmt.Printf("kk is running: http://%s/?token=%s\n", addr(), token)
	go func() {
		for range time.Tick(time.Second) {
			s.tick()
		}
	}()
	<-sig
	log.Print("kk stopping")
	s.inbox.stop()
	os.Remove(sockPath())
	return nil
}

var client = &http.Client{Transport: &http.Transport{
	DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sockPath())
	},
}}

func call(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rd = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, "http://kk"+path, rd)
	if id := os.Getenv("KK_CONVERSATION"); id != "" {
		req.Header.Set(agentHeader, id)
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("kk server is not running, start it with `kk`")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		json.Unmarshal(data, &e)
		return errors.New(e.Error)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// agentHeader carries the conversation of the agent that runs the kk CLI, so the server tells agents from the user.
const agentHeader = "X-KK-Agent"

func runCLI(cmd string, args []string) error {
	need := func(n int) error {
		if len(args) < n {
			return fmt.Errorf("missing arguments\n\n%s", usage)
		}
		return nil
	}
	switch cmd {
	case "url":
		token, err := os.ReadFile(filepath.Join(home(), "token"))
		if err != nil {
			return err
		}
		fmt.Printf("http://%s/?token=%s\n", addr(), bytes.TrimSpace(token))
	case "ls":
		var list []convView
		if err := call("GET", "/api/conversations", nil, &list); err != nil {
			return err
		}
		for _, c := range list {
			state := c.Status
			if c.Attention != "" {
				state = "ACTION REQUIRED"
			}
			waiting := c.WaitingFor
			if waiting == "" {
				waiting = "-"
			}
			model := c.Model
			if model == "" {
				model = "default"
			}
			fmt.Printf("%s  %-15s  %-9s  %-6s  %-14s  %s  %s\n", c.ID, state, waiting, c.Harness, model, lastActive(c.Conversation).Format("01-02 15:04"), c.Title)
		}
	case "show":
		if err := need(1); err != nil {
			return err
		}
		fs := flag.NewFlagSet("show", flag.ContinueOnError)
		n := fs.Int("n", 20, "events")
		fs.Parse(args[1:])
		var c convView
		if err := call("GET", "/api/conversations/"+args[0], nil, &c); err != nil {
			return err
		}
		fmt.Printf("%s · %s · %s · %s\n", c.Title, c.Harness, c.Dir, c.Status)
		if c.Attention != "" {
			fmt.Printf("ACTION REQUIRED: %s\n", c.Attention)
		}
		if c.WaitingFor != "" {
			fmt.Printf("Waiting for the %s\n", c.WaitingFor)
		}
		if c.Escalation != "" {
			fmt.Printf("Escalated to the user: %s\n", c.Escalation)
		}
		events := c.Events
		if len(events) > *n {
			events = events[len(events)-*n:]
		}
		for _, e := range events {
			fmt.Println()
			switch e.Kind {
			case kindTool:
				fmt.Printf("[tool %s] %s\n", e.Tool, e.Input)
				if e.Output != "" {
					fmt.Println(indent(clip(e.Output, 600)))
				}
			default:
				fmt.Printf("[%s] %s\n", e.Kind, clip(e.Text, 4000))
				for _, a := range e.Attachments {
					fmt.Printf("[attachment] %s\n", a.Path)
				}
			}
		}
	case "new":
		fs := flag.NewFlagSet("new", flag.ContinueOnError)
		model := fs.String("m", "", "model")
		dir := fs.String("C", ".", "directory")
		worktree := fs.Bool("w", false, "start in a new git worktree on its own branch")
		group := fs.String("g", "", "feature group")
		if err := need(1); err != nil {
			return err
		}
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		d, _ := filepath.Abs(*dir)
		var c convView
		if err := call("POST", "/api/conversations", map[string]any{"harness": args[0], "model": *model, "dir": d, "prompt": strings.Join(fs.Args(), " "), "worktree": *worktree, "group": *group}, &c); err != nil {
			return err
		}
		fmt.Println(c.ID)
	case "group":
		if len(args) == 0 {
			var groups []groupView
			if err := call("GET", "/api/groups", nil, &groups); err != nil {
				return err
			}
			for _, g := range groups {
				if g.Group == "" {
					g.Group = ungrouped
				}
				fmt.Printf("%s  %-*s  %d\n", g.Project, groupMaxLen, g.Group, g.Sessions)
			}
			return nil
		}
		if err := need(2); err != nil {
			return err
		}
		return call("POST", "/api/conversations/"+args[0]+"/group", map[string]string{"group": strings.Join(args[1:], " ")}, nil)
	case "send":
		if err := need(2); err != nil {
			return err
		}
		text := strings.Join(args[1:], " ")
		if text == "-" {
			data, _ := io.ReadAll(os.Stdin)
			text = string(data)
		}
		return call("POST", "/api/conversations/"+args[0]+"/send", map[string]string{"text": text}, nil)
	case "screen":
		if err := need(1); err != nil {
			return err
		}
		var out struct{ Screen string }
		if err := call("GET", "/api/conversations/"+args[0]+"/screen", nil, &out); err != nil {
			return err
		}
		fmt.Println(out.Screen)
	case "keys":
		if err := need(2); err != nil {
			return err
		}
		return call("POST", "/api/conversations/"+args[0]+"/keys", map[string][]string{"keys": args[1:]}, nil)
	case "resume", "close", "interrupt", "stop", "reset":
		if err := need(1); err != nil {
			return err
		}
		return call("POST", "/api/conversations/"+args[0]+"/"+cmd, struct{}{}, nil)
	case "escalate":
		if err := need(2); err != nil {
			return err
		}
		return call("POST", "/api/conversations/"+args[0]+"/escalate", map[string]string{"reason": strings.Join(args[1:], " ")}, nil)
	case "hook":
		return hook(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	return nil
}

// hook forwards an agent hook to the server. It never prints on success, because some harnesses add hook output to the model's context.
func hook(args []string) error {
	id := os.Getenv("KK_CONVERSATION")
	if len(args) < 1 || id == "" {
		return errors.New("usage: KK_CONVERSATION=<id> kk hook <event>")
	}
	var payload []byte
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice == 0 {
		payload, _ = io.ReadAll(io.LimitReader(os.Stdin, 16<<20))
	}
	req, _ := http.NewRequest("POST", "http://kk/api/hook/"+id+"/"+args[0], bytes.NewReader(payload))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ")
}
