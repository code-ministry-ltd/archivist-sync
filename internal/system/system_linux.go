//go:build !darwin && !windows

package system

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// OpenBrowser opens a link in the default browser.
func OpenBrowser(url string) error { return exec.Command("xdg-open", url).Start() }

// PickFolder opens zenity's or kdialog's folder picker; "" means it was cancelled.
func PickFolder(prompt string) (string, error) {
	if _, err := exec.LookPath("zenity"); err == nil {
		out, err := exec.Command("zenity", "--file-selection", "--directory", "--title="+prompt).Output()
		if err != nil {
			return "", nil
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := exec.LookPath("kdialog"); err == nil {
		out, err := exec.Command("kdialog", "--getexistingdirectory", os.Getenv("HOME"), "--title", prompt).Output()
		if err != nil {
			return "", nil
		}
		return strings.TrimSpace(string(out)), nil
	}
	return "", ErrNoPicker
}

// DeviceName is the computer's pretty name, or its host name.
func DeviceName() string {
	if raw, err := os.ReadFile("/etc/machine-info"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if v, ok := strings.CutPrefix(line, "PRETTY_HOSTNAME="); ok {
				return truncate(strings.Trim(v, `"`))
			}
		}
	}
	return truncate(hostname())
}

func configHome() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config")
}

func dataHome() string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return dir
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".local", "share")
}

func unitFile() string {
	return filepath.Join(configHome(), "systemd", "user", "archivist-sync.service")
}
func autostartFile() string {
	return filepath.Join(configHome(), "autostart", "archivist-sync.desktop")
}
func desktopFile() string {
	return filepath.Join(dataHome(), "applications", "archivist-sync.desktop")
}

func hasSystemd() bool {
	return exec.Command("systemctl", "--user", "show-environment").Run() == nil
}

// InstallLogin makes the program start at login: a systemd user service, or an XDG
// autostart entry where there's no systemd.
func InstallLogin(bin string) error {
	if hasSystemd() {
		unit := fmt.Sprintf(`[Unit]
Description=Archivist Sync

[Service]
ExecStart=%s run
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`, systemdQuote(bin))
		if err := os.MkdirAll(filepath.Dir(unitFile()), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(unitFile(), []byte(unit), 0o644); err != nil {
			return err
		}
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		return exec.Command("systemctl", "--user", "enable", "--now", "archivist-sync.service").Run()
	}
	entry := fmt.Sprintf("[Desktop Entry]\nType=Application\nName=Archivist Sync\nExec=%s run\nX-GNOME-Autostart-enabled=true\nNoDisplay=true\n", desktopQuote(bin))
	if err := os.MkdirAll(filepath.Dir(autostartFile()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(autostartFile(), []byte(entry), 0o644)
}

// UninstallLogin stops the program starting at login.
func UninstallLogin() error {
	if _, err := os.Stat(unitFile()); err == nil {
		_ = exec.Command("systemctl", "--user", "disable", "--now", "archivist-sync.service").Run()
		_ = os.Remove(unitFile())
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	}
	if err := os.Remove(autostartFile()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// InstallShortcut adds a desktop entry that opens the page.
func InstallShortcut(bin string) error {
	entry := fmt.Sprintf("[Desktop Entry]\nType=Application\nName=Archivist Sync\nComment=Choose the folders that sync to your archive\nExec=%s open\nTerminal=false\nCategories=Utility;\n", desktopQuote(bin))
	if err := os.MkdirAll(filepath.Dir(desktopFile()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(desktopFile(), []byte(entry), 0o644)
}

// UninstallShortcut removes the desktop entry.
func UninstallShortcut() error {
	if err := os.Remove(desktopFile()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Detach starts a process outside this one's session.
func Detach(cmd *exec.Cmd) { detachUnix(cmd) }

func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func desktopQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\\\`, `"`, `\"`, "`", "\\`", "$", `\$`).Replace(s) + `"`
}
