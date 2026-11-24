package main

import (
	"bytes"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type picked struct {
	// Dir is empty when the dialog was cancelled.
	Dir string `json:"dir"`
	// Native is false when this machine cannot show its folder dialog to the person asking, who then gets the folder browser in the page.
	Native bool `json:"native"`
}

// folderDialog is the command that opens the operating system's folder dialog in start and prints the chosen path.
// The path travels as an argument or in KK_PICK_DIR, never inside a script.
func folderDialog(start string) []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"osascript", "-e", "on run argv", "-e", "activate",
			"-e", `POSIX path of (choose folder with prompt "Choose the directory" default location POSIX file (item 1 of argv))`,
			"-e", "end run", "--", start}
	case "windows":
		return []string{"powershell", "-NoProfile", "-STA", "-Command",
			`Add-Type -AssemblyName System.Windows.Forms; $d = New-Object System.Windows.Forms.FolderBrowserDialog; $d.SelectedPath = $env:KK_PICK_DIR; if ($d.ShowDialog() -eq 'OK') { $d.SelectedPath }`}
	}
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return nil
	}
	if _, err := exec.LookPath("zenity"); err == nil {
		return []string{"zenity", "--file-selection", "--directory", "--title=Choose the directory", "--filename=" + start + "/"}
	}
	return []string{"kdialog", "--title", "Choose the directory", "--getexistingdirectory", start}
}

// sameMachine reports whether the request comes from a browser on the machine kk runs on, the only screen a dialog can open on.
func sameMachine(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback() && r.Header.Get("X-Forwarded-For") == ""
}

// handlePick opens the folder dialog of the operating system, starting in the given directory, and waits for the answer.
func handlePick(r *http.Request) (any, error) {
	var in struct {
		Dir string `json:"dir"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	start, err := expandDir(in.Dir)
	if err != nil {
		if start, err = expandDir(""); err != nil {
			return nil, err
		}
	}
	args := folderDialog(start)
	if args == nil || !sameMachine(r) {
		return picked{}, nil
	}
	// The dialog closes when the request ends, so a closed page does not leave it on the screen.
	cmd := exec.CommandContext(r.Context(), args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "KK_PICK_DIR="+start)
	out, err := cmd.Output()
	dir := strings.TrimSpace(string(out))
	var exit *exec.ExitError
	switch {
	case err == nil && dir != "":
		return picked{Dir: filepath.Clean(dir), Native: true}, nil
	case err == nil:
		return picked{Native: true}, nil
	// Cancel exits with 1 everywhere; osascript does the same for its errors, and names cancel -128.
	case errors.As(err, &exit) && exit.ExitCode() == 1 && (runtime.GOOS != "darwin" || bytes.Contains(exit.Stderr, []byte("(-128)"))):
		return picked{Native: true}, nil
	}
	log.Printf("folder dialog: %v", err)
	return picked{}, nil
}
