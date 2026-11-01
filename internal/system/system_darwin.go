package system

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const launchLabel = "cc.the-archivist.sync"

// OpenBrowser opens a link in the default browser.
func OpenBrowser(url string) error { return exec.Command("open", url).Start() }

// PickFolder opens macOS's own folder picker; "" means it was cancelled.
func PickFolder(prompt string) (string, error) {
	script := fmt.Sprintf(`POSIX path of (choose folder with prompt %s)`, strconv.Quote(prompt))
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		// Cancelling the dialog is an error to osascript (-128).
		return "", nil
	}
	return strings.TrimRight(strings.TrimSpace(string(out)), "/"), nil
}

// DeviceName is the computer's name as set in System Settings, such as "Jim's MacBook".
func DeviceName() string {
	if name, err := output("scutil", "--get", "ComputerName"); err == nil && name != "" {
		return truncate(name)
	}
	return truncate(hostname())
}

func home() string { h, _ := os.UserHomeDir(); return h }

func agentPlist() string {
	return filepath.Join(home(), "Library", "LaunchAgents", launchLabel+".plist")
}

func appBundle() string { return filepath.Join(home(), "Applications", "Archivist Sync.app") }

// InstallLogin makes the program start at login, with a LaunchAgent that keeps it running.
func InstallLogin(bin string) error {
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key><array><string>%s</string><string>run</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ProcessType</key><string>Background</string>
</dict>
</plist>
`, launchLabel, xmlEscape(bin))
	if err := os.MkdirAll(filepath.Dir(agentPlist()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(agentPlist(), []byte(plist), 0o644); err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain, agentPlist()).Run()
	if err := exec.Command("launchctl", "bootstrap", domain, agentPlist()).Run(); err != nil {
		return exec.Command("launchctl", "load", "-w", agentPlist()).Run()
	}
	return nil
}

// UninstallLogin stops the program starting at login.
func UninstallLogin() error {
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain, agentPlist()).Run()
	err := os.Remove(agentPlist())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// InstallShortcut makes an "Archivist Sync" app in ~/Applications that opens the page, so
// Spotlight finds it. Made on this computer, it isn't quarantined.
func InstallShortcut(bin string) error {
	contents := filepath.Join(appBundle(), "Contents")
	if err := os.MkdirAll(filepath.Join(contents, "MacOS"), 0o755); err != nil {
		return err
	}
	info := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>Archivist Sync</string>
  <key>CFBundleIdentifier</key><string>cc.the-archivist.sync.opener</string>
  <key>CFBundleExecutable</key><string>open-archivist-sync</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>LSUIElement</key><true/>
</dict>
</plist>
`
	if err := os.WriteFile(filepath.Join(contents, "Info.plist"), []byte(info), 0o644); err != nil {
		return err
	}
	script := fmt.Sprintf("#!/bin/sh\nexec %s open\n", shellQuote(bin))
	return os.WriteFile(filepath.Join(contents, "MacOS", "open-archivist-sync"), []byte(script), 0o755)
}

// UninstallShortcut removes the app made by InstallShortcut.
func UninstallShortcut() error { return os.RemoveAll(appBundle()) }

// Detach starts a process outside this one's session.
func Detach(cmd *exec.Cmd) { detachUnix(cmd) }

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
