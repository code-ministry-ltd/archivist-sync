// Package system does what differs between macOS, Windows and Linux: opening the browser, the
// system's folder picker, the computer's name, starting at login and the shortcut to the page
// (spec 0012 §3.2, §4).
package system

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNoPicker means this computer has no folder picker the program can open; type a path.
var ErrNoPicker = errors.New("no folder picker on this computer")

// Suggestions are the folders the page offers as shortcuts: Documents and Desktop, if present.
func Suggestions() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []string
	for _, name := range []string{"Documents", "Desktop"} {
		for _, p := range []string{filepath.Join(home, name), filepath.Join(home, "OneDrive", name)} {
			if info, err := os.Stat(p); err == nil && info.IsDir() {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// Executable is the running program's own path, with links resolved.
func Executable() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

func output(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "this computer"
	}
	return strings.TrimSuffix(h, ".local")
}

// truncate keeps a device name within the archive's 60 characters.
func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > 60 {
		return string([]rune(s)[:60])
	}
	if s == "" {
		return "this computer"
	}
	return s
}
