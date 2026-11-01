// Package daemon is Archivist Sync running in the background (spec 0012 §4 to §7): it watches
// the chosen folders, syncs them, signs the computer in, keeps itself up to date and serves the
// settings page.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/code-ministry-ltd/archivist-sync/internal/archive"
	"github.com/code-ministry-ltd/archivist-sync/internal/config"
	"github.com/code-ministry-ltd/archivist-sync/internal/rules"
	"github.com/code-ministry-ltd/archivist-sync/internal/syncer"
	"github.com/code-ministry-ltd/archivist-sync/internal/update"
)

// Timing is how often the program looks and when it syncs (spec 0012 §6.3). Tests shorten it.
type Timing struct {
	// Look is how often each folder is checked for changes.
	Look time.Duration
	// Settle is how long after the last change a sync waits.
	Settle time.Duration
	// Backstop is the longest a folder goes without a sync.
	Backstop time.Duration
	// UpdateEvery is how often it looks for a new release.
	UpdateEvery time.Duration
}

// DefaultTiming is a look every minute, a sync two minutes after changes settle and at least
// every fifteen minutes, and a daily update check.
var DefaultTiming = Timing{Look: time.Minute, Settle: 2 * time.Minute, Backstop: 15 * time.Minute, UpdateEvery: 24 * time.Hour}

// Hooks are what the daemon does to the computer, replaced in tests.
type Hooks struct {
	OpenBrowser func(url string) error
	PickFolder  func(prompt string) (string, error)
	DeviceName  func() string
	Suggestions func() []string
	// Uninstall removes the program from the computer; the daemon then exits.
	Uninstall func() error
	// Restart starts the updated program in place of this one.
	Restart func() error
	// Executable is where the running program is, for updates.
	Executable func() (string, error)
}

// FolderStatus is one folder as the page and `status` show it.
type FolderStatus struct {
	Path string `json:"path"`
	// Shown is the path for people: under the home folder, it starts with "~".
	Shown        string           `json:"shown"`
	Root         string           `json:"root"`
	State        string           `json:"state"`
	Error        string           `json:"error,omitempty"`
	LastSync     *time.Time       `json:"lastSync,omitempty"`
	Files        int              `json:"files"`
	LeftOutCount int              `json:"leftOutCount"`
	LeftOut      []syncer.LeftOut `json:"leftOut"`
	Waiting      int              `json:"waiting"`
}

type folder struct {
	FolderStatus
	stamps      map[string]syncer.Stamp
	lastChange  time.Time
	nextAttempt time.Time
	failures    int
	requested   bool
	allowLocal  bool
}

// Connecting is a sign-in waiting for approval in the browser.
type Connecting struct {
	URL       string    `json:"url"`
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Daemon is the running program.
type Daemon struct {
	store   *config.Store
	version string
	timing  Timing
	hooks   Hooks
	updates *update.Checker

	mu          sync.Mutex
	folders     map[string]*folder
	connecting  *Connecting
	connectNote string
	updateNote  string
	wake        chan struct{}
	quit        chan struct{}
	quitOnce    sync.Once
	syncing     sync.Mutex
}

// New makes a daemon over these settings.
func New(store *config.Store, version string, timing Timing, hooks Hooks, updates *update.Checker) *Daemon {
	d := &Daemon{
		store: store, version: version, timing: timing, hooks: hooks, updates: updates,
		folders: map[string]*folder{}, wake: make(chan struct{}, 1), quit: make(chan struct{}),
	}
	for _, f := range store.Get().Folders {
		d.folders[f.Root] = &folder{FolderStatus: FolderStatus{Path: f.Path, Root: f.Root, State: "starting"}}
	}
	return d
}

// Done is closed when the daemon has been asked to stop: uninstalled, or replaced by an update.
func (d *Daemon) Done() <-chan struct{} { return d.quit }

func (d *Daemon) stop() { d.quitOnce.Do(func() { close(d.quit) }) }

func (d *Daemon) client() *archive.Client {
	s := d.store.Get()
	return archive.New(s.Archive, s.Token, d.version)
}

func (d *Daemon) connected() bool { return d.store.Get().Token != "" }

// Wake asks for a look at every folder now.
func (d *Daemon) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Loop runs the schedule until the context ends: a look at each folder every Timing.Look, a
// sync where one is due, and an update check every Timing.UpdateEvery.
func (d *Daemon) Loop(ctx context.Context) {
	look := time.NewTicker(d.timing.Look)
	defer look.Stop()
	nextUpdate := time.Now().Add(time.Minute)
	for {
		d.lookAll(ctx)
		if d.updates != nil && time.Now().After(nextUpdate) {
			nextUpdate = time.Now().Add(d.timing.UpdateEvery)
			d.CheckUpdate(ctx, true)
		}
		select {
		case <-ctx.Done():
			return
		case <-d.quit:
			return
		case <-look.C:
		case <-d.wake:
		}
	}
}

func (d *Daemon) lookAll(ctx context.Context) {
	d.mu.Lock()
	roots := make([]string, 0, len(d.folders))
	for r := range d.folders {
		roots = append(roots, r)
	}
	d.mu.Unlock()
	sort.Strings(roots)
	for _, r := range roots {
		if ctx.Err() != nil {
			return
		}
		d.look(ctx, r)
	}
}

// look checks one folder for changes and syncs it if it's due.
func (d *Daemon) look(ctx context.Context, root string) {
	d.mu.Lock()
	f, ok := d.folders[root]
	if !ok {
		d.mu.Unlock()
		return
	}
	path := f.Path
	d.mu.Unlock()

	if !d.connected() {
		d.setState(root, "disconnected", "")
		return
	}
	stamps, err := syncer.Stamps(path)
	now := time.Now()
	d.mu.Lock()
	if errors.Is(err, syncer.ErrMissing) {
		f.State, f.Error = "missing", ""
		f.stamps = nil
		d.mu.Unlock()
		return
	}
	if f.stamps != nil && !reflect.DeepEqual(f.stamps, stamps) {
		f.lastChange = now
	}
	f.stamps = stamps
	due := f.requested || f.LastSync == nil ||
		(!f.lastChange.IsZero() && now.Sub(f.lastChange) >= d.timing.Settle) ||
		now.Sub(*f.LastSync) >= d.timing.Backstop
	if f.nextAttempt.After(now) && !f.requested {
		due = false
	}
	d.mu.Unlock()
	if due {
		d.syncFolder(ctx, root)
	}
}

func (d *Daemon) setState(root, state, msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if f, ok := d.folders[root]; ok {
		f.State, f.Error = state, msg
	}
}

// syncFolder runs one sync of a folder, one folder at a time.
func (d *Daemon) syncFolder(ctx context.Context, root string) {
	d.syncing.Lock()
	defer d.syncing.Unlock()
	d.mu.Lock()
	f, ok := d.folders[root]
	if !ok {
		d.mu.Unlock()
		return
	}
	f.State, f.Error, f.requested = "syncing", "", false
	path, allowLocal := f.Path, f.allowLocal
	d.mu.Unlock()

	c := d.client()
	allow := allowLocal
	if states, err := c.Roots(ctx); err == nil {
		for _, s := range states {
			if s.Root == root && s.AllowRemoval {
				allow = true
			}
		}
	}
	run := func() (syncer.Outcome, error) {
		return syncer.Run(ctx, syncer.Options{
			Client: c, Dir: path, Root: root, AllowRemoval: allow,
			Cache: syncer.OpenCache(d.cacheFile(root)),
		})
	}
	out, err := run()
	// After connecting again, this computer's new token claims its folders back.
	if archive.IsCode(err, "wrong_root") {
		if c.AddRoot(ctx, root, true) == nil {
			out, err = run()
		}
	}

	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	f, ok = d.folders[root]
	if !ok {
		return
	}
	if err != nil {
		if archive.IsRevoked(err) {
			d.mu.Unlock()
			d.forgetToken("This computer was disconnected from your archive. Connect it again.")
			d.mu.Lock()
			return
		}
		f.failures++
		backoff := min(time.Duration(1<<min(f.failures, 5))*time.Minute, 30*time.Minute)
		f.nextAttempt = now.Add(backoff)
		f.State, f.Error = "error", friendly(err)
		log.Printf("sync failed: %s", errorCode(err))
		return
	}
	f.failures = 0
	f.nextAttempt = time.Time{}
	f.lastChange = time.Time{}
	f.State = out.State
	f.Files, f.LeftOutCount, f.LeftOut = out.Scan.Files, out.Scan.LeftOutCount, out.Scan.LeftOut
	f.Waiting = out.Waiting
	if out.State == "synced" || out.State == "empty" {
		f.LastSync = &now
		f.allowLocal = false
	}
	if out.State == "waiting" && f.LastSync == nil {
		f.LastSync = &now
	}
	log.Printf("sync %s: sent %d, removed %d, waiting %d", out.State, out.Uploaded, out.Removed, out.Waiting)
}

// friendly turns an error into a line for the page, never naming files.
func friendly(err error) string {
	var ae *archive.Error
	if errors.As(err, &ae) {
		switch {
		case ae.Code == "archive_full":
			return "Your archive is full."
		case ae.Status == 429:
			return "Your archive asked to slow down; it tries again soon."
		case ae.Status >= 500:
			return "Your archive didn't answer; it tries again soon."
		}
		if ae.Message != "" {
			return ae.Message
		}
	}
	return "Couldn't reach your archive; it tries again soon."
}

// errorCode is an error's code for the log: never a path or a name.
func errorCode(err error) string {
	var ae *archive.Error
	if errors.As(err, &ae) {
		return fmt.Sprintf("%d %s", ae.Status, ae.Code)
	}
	return "network"
}

func (d *Daemon) cacheFile(root string) string {
	return filepath.Join(d.store.Dir(), "cache", root+".json")
}

func (d *Daemon) forgetToken(note string) {
	_ = d.store.Update(func(s *config.Settings) { s.Token = "" })
	d.mu.Lock()
	d.connectNote = note
	for _, f := range d.folders {
		f.State, f.Error = "disconnected", ""
	}
	d.mu.Unlock()
}

// --- Connecting (spec 0012 §5)

// Connect starts signing this computer in, or returns the sign-in already waiting.
func (d *Daemon) Connect(ctx context.Context) (*Connecting, error) {
	d.mu.Lock()
	if d.connecting != nil && time.Now().Before(d.connecting.ExpiresAt) {
		c := *d.connecting
		d.mu.Unlock()
		return &c, nil
	}
	d.mu.Unlock()
	device := d.hooks.DeviceName()
	c := d.client()
	c.Token = ""
	start, err := c.StartSignIn(ctx, device)
	if err != nil {
		return nil, err
	}
	conn := &Connecting{URL: start.VerificationURL, Code: start.UserCode, ExpiresAt: time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)}
	d.mu.Lock()
	d.connecting, d.connectNote = conn, ""
	d.mu.Unlock()
	_ = d.store.Update(func(s *config.Settings) { s.Device = device })
	go d.poll(c, start, conn)
	out := *conn
	return &out, nil
}

func (d *Daemon) poll(c *archive.Client, start archive.DeviceStart, conn *Connecting) {
	interval := time.Duration(max(start.Interval, 1)) * time.Second
	if start.Interval == 0 {
		interval = 50 * time.Millisecond
	}
	finish := func(note string) {
		d.mu.Lock()
		if d.connecting == conn {
			d.connecting, d.connectNote = nil, note
		}
		d.mu.Unlock()
	}
	for time.Now().Before(conn.ExpiresAt) {
		select {
		case <-d.quit:
			return
		case <-time.After(interval):
		}
		status, token, err := c.PollSignIn(context.Background(), start.DeviceCode)
		if err != nil {
			continue
		}
		switch status {
		case "approved":
			_ = d.store.Update(func(s *config.Settings) { s.Token = token })
			d.reclaim()
			finish("")
			d.Wake()
			return
		case "declined":
			finish("The connection was declined in your archive.")
			return
		case "expired":
			finish("The connection expired. Connect again.")
			return
		}
	}
	finish("The connection expired. Connect again.")
}

// reclaim tells the archive, after connecting again, that this computer still syncs its folders.
func (d *Daemon) reclaim() {
	c := d.client()
	for _, f := range d.store.Get().Folders {
		_ = c.AddRoot(context.Background(), f.Root, true)
	}
}

// Disconnect revokes this computer's token and forgets it; folders stop until it connects again.
func (d *Daemon) Disconnect(ctx context.Context) {
	if d.connected() {
		_ = d.client().RevokeSelf(ctx)
	}
	d.forgetToken("")
}

// --- Folders (spec 0012 §6.1)

// ErrNotConnected means the computer must be connected first.
var ErrNotConnected = errors.New("connect this computer to your archive first")

// Add starts syncing a folder. With no root it's named archivist-sync- and the folder's name,
// numbered if that's taken; existing continues a root already in the archive.
func (d *Daemon) Add(ctx context.Context, path, root string, existing bool) (FolderStatus, error) {
	if !d.connected() {
		return FolderStatus{}, ErrNotConnected
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return FolderStatus{}, err
	}
	abs = filepath.Clean(abs)
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return FolderStatus{}, fmt.Errorf("%s isn't a folder on this computer", abs)
	}
	for _, f := range d.store.Get().Folders {
		if f.Path == abs || within(abs, f.Path) || within(f.Path, abs) {
			return FolderStatus{}, fmt.Errorf("%s overlaps a folder already syncing (%s)", abs, f.Path)
		}
	}
	c := d.client()
	named := root != ""
	if !named {
		root = rules.RootFor(filepath.Base(abs))
	}
	if !rules.ValidRoot(root) {
		return FolderStatus{}, fmt.Errorf("%q can't be a folder name in your archive: lower-case letters, digits, dots, dashes or underscores", root)
	}
	base := root
	for n := 2; ; n++ {
		err = c.AddRoot(ctx, root, existing)
		if err == nil || named || existing || !archive.IsCode(err, "root_exists") || n > 50 {
			break
		}
		root = fmt.Sprintf("%s-%d", strings.TrimRight(base[:min(len(base), 60)], "-"), n)
	}
	if err != nil {
		return FolderStatus{}, err
	}
	if err := d.store.Update(func(s *config.Settings) {
		s.Folders = append(s.Folders, config.Folder{Path: abs, Root: root})
	}); err != nil {
		return FolderStatus{}, err
	}
	d.mu.Lock()
	f := &folder{FolderStatus: FolderStatus{Path: abs, Root: root, State: "starting"}, requested: true}
	d.folders[root] = f
	status := f.FolderStatus
	d.mu.Unlock()
	d.Wake()
	return status, nil
}

func within(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

// Remove stops syncing a folder, by its path or its root; what's archived stays.
func (d *Daemon) Remove(ctx context.Context, which string) (string, error) {
	root := d.find(which)
	if root == "" {
		return "", fmt.Errorf("no folder %q is syncing", which)
	}
	if d.connected() {
		if err := d.client().RemoveRoot(ctx, root); err != nil && !archive.IsCode(err, "not_found") && !archive.IsRevoked(err) {
			return "", err
		}
	}
	_ = d.store.Update(func(s *config.Settings) {
		kept := s.Folders[:0]
		for _, f := range s.Folders {
			if f.Root != root {
				kept = append(kept, f)
			}
		}
		s.Folders = kept
	})
	d.mu.Lock()
	delete(d.folders, root)
	d.mu.Unlock()
	_ = os.Remove(d.cacheFile(root))
	return root, nil
}

// find names a folder's root from its root or its path.
func (d *Daemon) find(which string) string {
	abs, _ := filepath.Abs(which)
	for _, f := range d.store.Get().Folders {
		if f.Root == which || f.Path == which || f.Path == filepath.Clean(abs) {
			return f.Root
		}
	}
	return ""
}

// SyncNow asks for a sync of one folder, or of all with "".
func (d *Daemon) SyncNow(which string) error {
	d.mu.Lock()
	if which == "" {
		for _, f := range d.folders {
			f.requested = true
		}
		d.mu.Unlock()
		d.Wake()
		return nil
	}
	d.mu.Unlock()
	root := d.find(which)
	d.mu.Lock()
	f, ok := d.folders[root]
	if ok {
		f.requested = true
	}
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("no folder %q is syncing", which)
	}
	d.Wake()
	return nil
}

// AllowRemoval is Remove them on the page: the next sync removes what the guard held.
func (d *Daemon) AllowRemoval(which string) error {
	root := d.find(which)
	d.mu.Lock()
	f, ok := d.folders[root]
	if ok {
		f.allowLocal, f.requested = true, true
	}
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("no folder %q is syncing", which)
	}
	d.Wake()
	return nil
}

// --- Status

// Status is what the page and `status` show.
type Status struct {
	Version     string         `json:"version"`
	Archive     string         `json:"archive"`
	Device      string         `json:"device"`
	Connected   bool           `json:"connected"`
	Connecting  *Connecting    `json:"connecting"`
	ConnectNote string         `json:"connectNote,omitempty"`
	UpdateNote  string         `json:"updateNote,omitempty"`
	Folders     []FolderStatus `json:"folders"`
}

// Status reports the program's state.
func (d *Daemon) Status() Status {
	s := d.store.Get()
	d.mu.Lock()
	defer d.mu.Unlock()
	out := Status{Version: d.version, Archive: s.Archive, Device: s.Device, Connected: s.Token != "",
		ConnectNote: d.connectNote, UpdateNote: d.updateNote, Folders: []FolderStatus{}}
	if d.connecting != nil {
		c := *d.connecting
		out.Connecting = &c
	}
	if out.Device == "" {
		out.Device = d.hooks.DeviceName()
	}
	for _, f := range d.folders {
		fs := f.FolderStatus
		fs.Shown = shown(fs.Path)
		if fs.LeftOut == nil {
			fs.LeftOut = []syncer.LeftOut{}
		}
		out.Folders = append(out.Folders, fs)
	}
	sort.Slice(out.Folders, func(i, j int) bool { return out.Folders[i].Path < out.Folders[j].Path })
	return out
}

// --- Updates (spec 0012 §7)

// CheckUpdate looks for a newer release and installs it; quiet hides "up to date".
func (d *Daemon) CheckUpdate(ctx context.Context, quiet bool) string {
	note := func(s string) string {
		d.mu.Lock()
		d.updateNote = s
		d.mu.Unlock()
		return s
	}
	if d.updates == nil || d.version == "dev" {
		return note("This is a development build; it doesn't update itself.")
	}
	r, err := d.updates.Latest(ctx)
	if err != nil {
		if quiet {
			return ""
		}
		return note("Couldn't check for updates; it tries again tomorrow.")
	}
	if !update.Newer(r.Version, d.version) {
		if quiet {
			return ""
		}
		return note("Up to date.")
	}
	bin, err := d.updates.Fetch(ctx, r)
	if err != nil {
		log.Printf("update not installed: %v", err)
		return note("An update couldn't be installed; it tries again tomorrow.")
	}
	exe, err := d.hooks.Executable()
	if err == nil {
		// Wait for any sync to finish first.
		d.syncing.Lock()
		err = update.Replace(exe, bin)
		d.syncing.Unlock()
	}
	if err != nil {
		return note("An update couldn't be installed; it tries again tomorrow.")
	}
	log.Printf("updated to %s", r.Version)
	if d.hooks.Restart != nil {
		go func() {
			time.Sleep(500 * time.Millisecond)
			if d.hooks.Restart() == nil {
				d.stop()
			}
		}()
	}
	return note("Updated to " + r.Version + ".")
}

// Uninstall disconnects, removes the program from the computer and stops it (spec 0012 §7).
func (d *Daemon) Uninstall(ctx context.Context) error {
	d.Disconnect(ctx)
	if d.hooks.Uninstall != nil {
		if err := d.hooks.Uninstall(); err != nil {
			return err
		}
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		d.stop()
	}()
	return nil
}

func suggestRoot(path string) string { return rules.RootFor(filepath.Base(filepath.Clean(path))) }

// shown writes a path under the home folder as ~/…, for people to read.
func shown(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join("~", rel)
	}
	return path
}
