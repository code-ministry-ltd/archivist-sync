// Command archivist-sync keeps folders on this computer in The Archivist (spec 0012). Installed
// once, it runs in the background; its settings are a page in the browser. Every command is
// non-interactive, prints short plain lines (or JSON with --json), and exits 0 on success, so a
// person or their AI assistant can drive it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/code-ministry-ltd/archivist-sync/internal/archive"
	"github.com/code-ministry-ltd/archivist-sync/internal/config"
	"github.com/code-ministry-ltd/archivist-sync/internal/daemon"
	"github.com/code-ministry-ltd/archivist-sync/internal/system"
	"github.com/code-ministry-ltd/archivist-sync/internal/update"
)

// version is set at release time with -ldflags "-X main.version=1.0.0".
var version = "dev"

const usage = `Archivist Sync keeps folders on this computer in your archive.

Usage:
  archivist-sync open                 open the settings page in your browser
  archivist-sync connect              connect this computer to your archive (prints a link to approve)
  archivist-sync add <folder>         start syncing a folder
      --name <name>                   its folder name in your archive (default archivist-sync-<folder>)
      --into <name>                   continue a folder already in your archive, such as one synced before
  archivist-sync remove <folder>      stop syncing a folder; what's in your archive stays
  archivist-sync status               what's syncing
  archivist-sync sync [<folder>]      sync now
  archivist-sync update               install the latest version now
  archivist-sync stop                 stop it until the next login (installing again does this)
  archivist-sync uninstall            remove Archivist Sync from this computer
  archivist-sync version

Add --json to any command for structured output.
`

type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func fail(msg string) error { return &exitError{code: 1, msg: msg} }

func main() {
	args := os.Args[1:]
	asJSON := false
	var rest []string
	for _, a := range args {
		if a == "--json" {
			asJSON = true
		} else {
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 {
		rest = []string{"help"}
	}
	err := runCommand(rest[0], rest[1:], asJSON)
	if err != nil {
		code := 1
		var ee *exitError
		if errors.As(err, &ee) {
			code = ee.code
		}
		if asJSON {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"error": err.Error()})
		} else {
			fmt.Fprintln(os.Stderr, err.Error())
		}
		os.Exit(code)
	}
}

func runCommand(cmd string, args []string, asJSON bool) error {
	switch cmd {
	case "run":
		return runDaemon()
	case "start":
		_, err := ensureRunning()
		return err
	case "install":
		return install(args, asJSON)
	case "open":
		return open(asJSON)
	case "connect":
		return connect(args, asJSON)
	case "add":
		return add(args, asJSON)
	case "remove":
		if len(args) != 1 {
			return fail("Name the folder to stop syncing: archivist-sync remove <folder>")
		}
		var out map[string]string
		if err := call(http.MethodDelete, "/api/folders?folder="+queryEscape(args[0]), nil, &out); err != nil {
			return err
		}
		return say(asJSON, out, fmt.Sprintf("Stopped syncing %s. What's in your archive stays.", out["root"]))
	case "status":
		return status(asJSON)
	case "sync":
		folder := ""
		if len(args) > 0 {
			folder = args[0]
		}
		if err := call(http.MethodPost, "/api/sync?folder="+queryEscape(folder), nil, nil); err != nil {
			return err
		}
		return say(asJSON, map[string]bool{"ok": true}, "Syncing now.")
	case "update":
		var out map[string]string
		if err := call(http.MethodPost, "/api/update", nil, &out); err != nil {
			return err
		}
		return say(asJSON, out, out["note"])
	case "uninstall":
		return uninstall(asJSON)
	case "stop":
		return stop(asJSON)
	case "version", "--version", "-v":
		return say(asJSON, map[string]string{"version": version}, "archivist-sync "+version)
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	default:
		return fail(fmt.Sprintf("Unknown command %q. Run archivist-sync help.", cmd))
	}
}

func say(asJSON bool, v any, line string) error {
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(v)
	}
	if line != "" {
		fmt.Println(line)
	}
	return nil
}

func queryEscape(s string) string {
	return strings.NewReplacer("%", "%25", "&", "%26", "+", "%2B", " ", "%20", "#", "%23", "?", "%3F").Replace(s)
}

func openStore() (*config.Store, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	return config.Open(dir)
}

// --- The background program

func runDaemon() error {
	store, err := openStore()
	if err != nil {
		return err
	}
	logFile := filepath.Join(store.Dir(), "archivist-sync.log")
	if info, err := os.Stat(logFile); err == nil && info.Size() > 1<<20 {
		_ = os.Rename(logFile, logFile+".1")
	}
	if f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		log.SetOutput(f)
	}

	// After an update the old copy may still hold the port for a moment.
	deadline := time.Now().Add(10 * time.Second)
	for {
		l, port, err := daemon.Listen(store.Get().Port)
		if err == nil {
			defer l.Close()
			exe, _ := system.Executable()
			update.CleanUp(exe)
			d := daemon.New(store, version, daemon.DefaultTiming, hooks(store), update.NewChecker())
			_ = store.Update(func(s *config.Settings) { s.Port = port })
			srv := &http.Server{Handler: daemon.NewServer(d, port).Handler(), ReadHeaderTimeout: 10 * time.Second}
			go func() { _ = srv.Serve(l) }()
			log.Printf("archivist-sync %s running on port %d", version, port)
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go d.Loop(ctx)
			select {
			case <-ctx.Done():
			case <-d.Done():
			}
			shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdown)
			return nil
		}
		if errors.Is(err, daemon.ErrRunning) && os.Getenv("ARCHIVIST_SYNC_RESTART") != "" && time.Now().Before(deadline) {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if errors.Is(err, daemon.ErrRunning) {
			return nil
		}
		return err
	}
}

func hooks(store *config.Store) daemon.Hooks {
	return daemon.Hooks{
		OpenBrowser: system.OpenBrowser,
		PickFolder:  system.PickFolder,
		DeviceName:  system.DeviceName,
		Suggestions: system.Suggestions,
		Executable:  system.Executable,
		Restart: func() error {
			exe, err := system.Executable()
			if err != nil {
				return err
			}
			return system.Restart(exe, []string{"run"})
		},
		Uninstall: func() error {
			_ = system.UninstallLogin()
			_ = system.UninstallShortcut()
			exe, err := system.Executable()
			if err == nil {
				_ = removeCommandLink(exe)
				_ = system.ScheduleRemoval(exe)
			}
			return os.RemoveAll(store.Dir())
		},
	}
}

// removeCommandLink removes the link the Linux install puts in ~/.local/bin.
func removeCommandLink(exe string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	link := filepath.Join(home, ".local", "bin", "archivist-sync")
	if target, err := os.Readlink(link); err == nil && target == exe {
		return os.Remove(link)
	}
	return nil
}

// --- Talking to the background program

type running struct {
	base string
	key  string
}

// ensureRunning starts the background program if it isn't running, and waits for it.
func ensureRunning() (running, error) {
	store, err := openStore()
	if err != nil {
		return running{}, err
	}
	s := store.Get()
	port := s.Port
	if port == 0 {
		port = daemon.DefaultPort
	}
	if daemon.Running(port) {
		return running{base: "http://127.0.0.1:" + strconv.Itoa(port), key: s.Key}, nil
	}
	exe, err := system.Executable()
	if err != nil {
		return running{}, err
	}
	cmd := exec.Command(exe, "run")
	system.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return running{}, fail("Couldn't start Archivist Sync: " + err.Error())
	}
	_ = cmd.Process.Release()
	for range 40 {
		time.Sleep(250 * time.Millisecond)
		// The background program saves its port to the settings file; read it afresh.
		if fresh, err := openStore(); err == nil {
			s = fresh.Get()
		}
		if s.Port != 0 && daemon.Running(s.Port) {
			return running{base: "http://127.0.0.1:" + strconv.Itoa(s.Port), key: s.Key}, nil
		}
	}
	return running{}, fail("Archivist Sync didn't start. Run it again, or reinstall it.")
}

func call(method, path string, in, out any) error {
	r, err := ensureRunning()
	if err != nil {
		return err
	}
	var body io.Reader
	if in != nil {
		raw, _ := json.Marshal(in)
		body = strings.NewReader(string(raw))
	}
	req, _ := http.NewRequest(method, r.base+path, body)
	req.Header.Set(daemon.KeyHeader, r.key)
	if in != nil {
		req.Header.Set("content-type", "application/json")
	}
	res, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return fail("Couldn't reach Archivist Sync: " + err.Error())
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("Archivist Sync answered %d.", res.StatusCode)
		}
		return fail(e.Error)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// --- Commands

func install(args []string, asJSON bool) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	noOpen := fs.Bool("no-open", false, "don't open the page")
	if err := fs.Parse(args); err != nil {
		return fail(err.Error())
	}
	exe, err := system.Executable()
	if err != nil {
		return err
	}
	if err := system.InstallLogin(exe); err != nil {
		return fail("Couldn't set Archivist Sync to start at login: " + err.Error())
	}
	if err := system.InstallShortcut(exe); err != nil {
		return fail("Couldn't add the Archivist Sync shortcut: " + err.Error())
	}
	if _, err := ensureRunning(); err != nil {
		return err
	}
	if !*noOpen {
		if err := open(true); err != nil {
			return err
		}
	}
	return say(asJSON, map[string]string{"installed": exe},
		"Archivist Sync is installed and running. It starts by itself when you log in.\nIts page is open in your browser: connect, then choose your folders.\nCommand: "+exe)
}

func open(quiet bool) error {
	r, err := ensureRunning()
	if err != nil {
		return err
	}
	url := r.base + "/?key=" + r.key
	if err := system.OpenBrowser(url); err != nil {
		return fail("Couldn't open the browser. Open this link: " + url)
	}
	if !quiet {
		fmt.Println("Opened Archivist Sync in your browser.")
	}
	return nil
}

func status(asJSON bool) error {
	var s daemon.Status
	if err := call(http.MethodGet, "/api/status", nil, &s); err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(s)
	}
	if s.Connected {
		fmt.Printf("Connected as %s to %s.\n", s.Device, s.Archive)
	} else {
		fmt.Println("Not connected. Run: archivist-sync connect")
	}
	if len(s.Folders) == 0 {
		fmt.Println("No folders syncing. Add one: archivist-sync add <folder>")
	}
	for _, f := range s.Folders {
		when := "not yet"
		if f.LastSync != nil {
			when = f.LastSync.Local().Format("2 Jan 15:04")
		}
		line := fmt.Sprintf("%s → %s: %s, %d files, last synced %s", f.Path, f.Root, f.State, f.Files, when)
		if f.LeftOutCount > 0 {
			line += fmt.Sprintf(", %d left out", f.LeftOutCount)
		}
		if f.Waiting > 0 {
			line += fmt.Sprintf(", %d removals waiting for you on the page", f.Waiting)
		}
		if f.Error != "" {
			line += " (" + f.Error + ")"
		}
		fmt.Println(line)
	}
	return nil
}

func connect(args []string, asJSON bool) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	noOpen := fs.Bool("no-open", false, "don't open the link in the browser")
	wait := fs.Duration("wait", 10*time.Minute, "how long to wait for approval")
	if err := fs.Parse(args); err != nil {
		return fail(err.Error())
	}
	var s daemon.Status
	if err := call(http.MethodGet, "/api/status", nil, &s); err != nil {
		return err
	}
	if s.Connected {
		return say(asJSON, map[string]any{"status": "connected", "device": s.Device}, "Already connected as "+s.Device+".")
	}
	q := "?open=1"
	if *noOpen {
		q = ""
	}
	var c daemon.Connecting
	if err := call(http.MethodPost, "/api/connect"+q, nil, &c); err != nil {
		return err
	}
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "pending", "url": c.URL, "code": c.Code})
	} else {
		fmt.Printf("Approve this computer in your archive: %s\nCheck the page shows the code %s. Waiting…\n", c.URL, c.Code)
	}
	deadline := time.Now().Add(*wait)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if err := call(http.MethodGet, "/api/status", nil, &s); err != nil {
			return err
		}
		if s.Connected {
			return say(asJSON, map[string]any{"status": "connected", "device": s.Device}, "connected")
		}
		if s.Connecting == nil && s.ConnectNote != "" {
			return fail(s.ConnectNote)
		}
	}
	return fail("Still waiting for approval. Run archivist-sync connect again once it's approved.")
}

func add(args []string, asJSON bool) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	name := fs.String("name", "", "its folder name in your archive")
	into := fs.String("into", "", "continue this folder already in your archive")
	// Flags may follow the folder, as people and assistants tend to write them.
	var folder string
	var flags []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			flags = append(flags, args[i])
			if !strings.Contains(args[i], "=") && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		} else if folder == "" {
			folder = args[i]
		}
	}
	if err := fs.Parse(flags); err != nil {
		return fail(err.Error())
	}
	if folder == "" {
		return fail("Name the folder to sync: archivist-sync add <folder>")
	}
	if strings.HasPrefix(folder, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			folder = filepath.Join(home, strings.TrimPrefix(folder, "~"))
		}
	}
	abs, err := filepath.Abs(folder)
	if err != nil {
		return err
	}
	root, existing := *name, false
	if *into != "" {
		root, existing = *into, true
	}
	var preview struct {
		Files        int `json:"files"`
		LeftOutCount int `json:"leftOutCount"`
	}
	if err := call(http.MethodGet, "/api/preview?path="+queryEscape(abs), nil, &preview); err != nil {
		return err
	}
	var f daemon.FolderStatus
	if err := call(http.MethodPost, "/api/folders", map[string]any{"path": abs, "root": root, "existing": existing}, &f); err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"path": f.Path, "root": f.Root, "files": preview.Files, "leftOut": preview.LeftOutCount})
	}
	line := fmt.Sprintf("Syncing %s to %s in your archive: %d files", f.Path, f.Root, preview.Files)
	if preview.LeftOutCount > 0 {
		line += fmt.Sprintf(", %d left out (see archivist-sync status)", preview.LeftOutCount)
	}
	fmt.Println(line + ".")
	return nil
}

func uninstall(asJSON bool) error {
	store, err := openStore()
	if err != nil {
		return err
	}
	if s := store.Get(); s.Port != 0 && daemon.Running(s.Port) {
		if err := call(http.MethodPost, "/api/uninstall", nil, nil); err != nil {
			return err
		}
	} else {
		if s := store.Get(); s.Token != "" {
			_ = archive.New(s.Archive, s.Token, version).RevokeSelf(context.Background())
		}
		if err := hooks(store).Uninstall(); err != nil {
			return err
		}
	}
	return say(asJSON, map[string]bool{"uninstalled": true}, "Archivist Sync has been removed from this computer. What it synced stays in your archive.")
}

// stop asks the background program to exit, and waits until it has.
func stop(asJSON bool) error {
	store, err := openStore()
	if err != nil {
		return err
	}
	s := store.Get()
	if s.Port == 0 || !daemon.Running(s.Port) {
		return say(asJSON, map[string]bool{"stopped": true}, "Archivist Sync isn't running.")
	}
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(s.Port)+"/api/stop", nil)
	req.Header.Set(daemon.KeyHeader, s.Key)
	if res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req); err == nil {
		res.Body.Close()
	}
	for range 40 {
		if !daemon.Running(s.Port) {
			return say(asJSON, map[string]bool{"stopped": true}, "Stopped Archivist Sync. It starts again when you log in.")
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fail("Archivist Sync didn't stop.")
}
