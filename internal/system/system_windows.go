//go:build windows

package system

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const runKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

// OpenBrowser opens a link in the default browser.
func OpenBrowser(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

func hidden(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	return cmd
}

// PickFolder opens Windows's folder browser through PowerShell; "" means it was cancelled.
func PickFolder(prompt string) (string, error) {
	script := `Add-Type -AssemblyName System.Windows.Forms;` +
		`$d = New-Object System.Windows.Forms.FolderBrowserDialog;` +
		`$d.Description = '` + strings.ReplaceAll(prompt, "'", "''") + `';` +
		`$d.ShowNewFolderButton = $false;` +
		`$f = New-Object System.Windows.Forms.Form -Property @{TopMost = $true};` +
		`if ($d.ShowDialog($f) -eq 'OK') { [Console]::Out.Write($d.SelectedPath) }`
	out, err := hidden(exec.Command("powershell", "-NoProfile", "-STA", "-Command", script)).Output()
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

// DeviceName is the computer's name.
func DeviceName() string {
	if name := os.Getenv("COMPUTERNAME"); name != "" {
		return truncate(name)
	}
	return truncate(hostname())
}

// InstallLogin makes the program start at login, with a per-user Run entry.
func InstallLogin(bin string) error {
	// "start" launches the background program with no window and exits, so login shows at most
	// a brief flash.
	return hidden(exec.Command("reg", "add", runKey, "/v", "Archivist Sync", "/t", "REG_SZ",
		"/d", `"`+bin+`" start`, "/f")).Run()
}

// UninstallLogin removes the Run entry.
func UninstallLogin() error {
	_ = hidden(exec.Command("reg", "delete", runKey, "/v", "Archivist Sync", "/f")).Run()
	return nil
}

func shortcutPath() string {
	return filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "Archivist Sync.lnk")
}

// InstallShortcut adds a Start menu shortcut that opens the page.
func InstallShortcut(bin string) error {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	script := `$s = (New-Object -ComObject WScript.Shell).CreateShortcut(` + q(shortcutPath()) + `);` +
		`$s.TargetPath = ` + q(bin) + `; $s.Arguments = 'open';` +
		`$s.Description = 'Choose the folders that sync to your archive'; $s.WindowStyle = 7; $s.Save()`
	return hidden(exec.Command("powershell", "-NoProfile", "-Command", script)).Run()
}

// UninstallShortcut removes the Start menu shortcut.
func UninstallShortcut() error {
	if err := os.Remove(shortcutPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Detach starts a process with no console, outside this one.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000008 | 0x00000200} // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
}

// ScheduleRemoval deletes a file a few seconds after this program exits: Windows won't delete
// a running program.
func ScheduleRemoval(path string) error {
	cmd := exec.Command("cmd", "/c", "ping -n 4 127.0.0.1 >nul & del /f /q \""+path+"\"")
	Detach(cmd)
	return cmd.Start()
}

// Restart starts the program at exe with args, with no window; the caller then exits.
func Restart(exe string, args []string) error {
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "ARCHIVIST_SYNC_RESTART=1")
	Detach(cmd)
	return cmd.Start()
}
