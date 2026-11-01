package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/code-ministry-ltd/archivist-sync/internal/archive/archivetest"
	"github.com/code-ministry-ltd/archivist-sync/internal/config"
)

type world struct {
	fake    *archivetest.Fake
	store   *config.Store
	d       *Daemon
	mu      sync.Mutex
	opened  []string
	stopped context.CancelFunc
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{fake: archivetest.New()}
	t.Cleanup(w.fake.Close)
	t.Setenv("ARCHIVIST_URL", w.fake.URL)
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w.store = store
	w.d = New(store, "1.0.0", Timing{Look: 20 * time.Millisecond, Settle: 60 * time.Millisecond, Backstop: time.Hour, UpdateEvery: time.Hour}, Hooks{
		OpenBrowser: func(url string) error { w.mu.Lock(); w.opened = append(w.opened, url); w.mu.Unlock(); return nil },
		PickFolder:  func(string) (string, error) { return "", nil },
		DeviceName:  func() string { return "Test Mac" },
		Suggestions: func() []string { return nil },
		Executable:  func() (string, error) { return "", os.ErrNotExist },
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	w.stopped = cancel
	t.Cleanup(cancel)
	go w.d.Loop(ctx)
	return w
}

func (w *world) connect(t *testing.T) {
	t.Helper()
	if _, err := w.d.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "connected", func() bool { return w.d.Status().Connected })
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 200 {
		if ok() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("never %s", what)
}

func write(t *testing.T, dir, rel, text string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (w *world) docs(root string) map[string]string {
	out := map[string]string{}
	w.fake.Lock(func() {
		for k, v := range w.fake.Docs[root] {
			out[k] = v
		}
	})
	return out
}

func TestConnectsOnceAndSyncsAnAddedFolderThenItsChanges(t *testing.T) {
	w := newWorld(t)
	if _, err := w.d.Add(context.Background(), t.TempDir(), "", false); err != ErrNotConnected {
		t.Fatalf("added before connecting: %v", err)
	}
	w.connect(t)
	if w.store.Get().Token != w.fake.Token || w.store.Get().Device != "Test Mac" {
		t.Fatal("the token or device name wasn't kept")
	}
	dir := filepath.Join(t.TempDir(), "Documents")
	write(t, dir, "letters/thanks.md", "# Thanks")
	f, err := w.d.Add(context.Background(), dir, "", false)
	if err != nil || f.Root != "archivist-sync-documents" {
		t.Fatalf("add: %+v %v", f, err)
	}
	eventually(t, "synced the folder", func() bool { return len(w.docs("archivist-sync-documents")) == 1 })

	write(t, dir, "letters/more.md", "# More")
	eventually(t, "synced the change after it settled", func() bool { return len(w.docs("archivist-sync-documents")) == 2 })
	st := w.d.Status()
	if len(st.Folders) != 1 || st.Folders[0].State != "synced" || st.Folders[0].Files != 2 || st.Folders[0].LastSync == nil {
		t.Errorf("status: %+v", st.Folders)
	}
}

func TestNamesAClashingFolderWithANumberAndContinuesOneOnRequest(t *testing.T) {
	w := newWorld(t)
	w.connect(t)
	w.fake.Lock(func() { w.fake.Docs["archivist-sync-documents"] = map[string]string{"old.md": "x"} })
	a := filepath.Join(t.TempDir(), "Documents")
	_ = os.MkdirAll(a, 0o755)
	f, err := w.d.Add(context.Background(), a, "", false)
	if err != nil || f.Root != "archivist-sync-documents-2" {
		t.Fatalf("numbered: %+v %v", f, err)
	}
	b := filepath.Join(t.TempDir(), "Documents")
	_ = os.MkdirAll(b, 0o755)
	if _, err := w.d.Add(context.Background(), b, "archivist-sync-documents", false); err == nil {
		t.Fatal("a named folder that exists was taken without asking")
	}
	if f, err := w.d.Add(context.Background(), b, "archivist-sync-documents", true); err != nil || f.Root != "archivist-sync-documents" {
		t.Fatalf("continue: %+v %v", f, err)
	}
}

func TestRefusesOverlappingFoldersAndStopsSyncingOne(t *testing.T) {
	w := newWorld(t)
	w.connect(t)
	dir := filepath.Join(t.TempDir(), "Papers")
	write(t, dir, "Tax/a.md", "a")
	if _, err := w.d.Add(context.Background(), dir, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := w.d.Add(context.Background(), filepath.Join(dir, "Tax"), "", false); err == nil {
		t.Error("a folder inside a syncing folder was added")
	}
	eventually(t, "synced", func() bool { return len(w.docs("archivist-sync-papers")) == 1 })
	if root, err := w.d.Remove(context.Background(), dir); err != nil || root != "archivist-sync-papers" {
		t.Fatalf("remove: %s %v", root, err)
	}
	if len(w.store.Get().Folders) != 0 || len(w.docs("archivist-sync-papers")) != 1 {
		t.Error("the folder wasn't forgotten, or its documents went")
	}
	w.fake.Lock(func() {
		if _, ok := w.fake.Roots["archivist-sync-papers"]; ok {
			t.Error("the archive wasn't told")
		}
	})
}

func TestARevokedTokenDisconnects(t *testing.T) {
	w := newWorld(t)
	w.connect(t)
	dir := t.TempDir()
	write(t, dir, "a.md", "a")
	if _, err := w.d.Add(context.Background(), dir, "", false); err != nil {
		t.Fatal(err)
	}
	w.fake.Lock(func() { w.fake.Revoked = true })
	_ = w.d.SyncNow("")
	eventually(t, "noticed the revoked token", func() bool { return !w.d.Status().Connected })
	if !strings.Contains(w.d.Status().ConnectNote, "Connect it again") {
		t.Errorf("note: %q", w.d.Status().ConnectNote)
	}
}

func TestThePageAnswersOnlyHereWithItsKeyAndOnlyToItself(t *testing.T) {
	w := newWorld(t)
	s := NewServer(w.d, 47811)
	h := s.Handler()
	key := w.store.Get().Key
	do := func(method, target string, headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, nil)
		r.Host = "127.0.0.1:47811"
		for k, v := range headers {
			if k == "Host" {
				r.Host = v
			} else {
				r.Header.Set(k, v)
			}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	if code := do("GET", "/api/status", map[string]string{"Host": "evil.example:47811", KeyHeader: key}).Code; code != http.StatusMisdirectedRequest {
		t.Errorf("another host: %d", code)
	}
	if code := do("GET", "/api/status", nil).Code; code != 401 {
		t.Errorf("no key: %d", code)
	}
	if code := do("GET", "/api/status", map[string]string{KeyHeader: key}).Code; code != 200 {
		t.Errorf("the commands' key: %d", code)
	}
	cookie := "archivist_sync_key=" + key
	if code := do("POST", "/api/sync", map[string]string{"Cookie": cookie, "Origin": "https://evil.example"}).Code; code != 403 {
		t.Errorf("a change from another site: %d", code)
	}
	if code := do("POST", "/api/sync", map[string]string{"Cookie": cookie, "Origin": "http://127.0.0.1:47811"}).Code; code != 202 {
		t.Errorf("a change from the page: %d", code)
	}
	if code := do("POST", "/api/sync", map[string]string{"Cookie": cookie}).Code; code != 403 {
		t.Errorf("a cookie change with no origin: %d", code)
	}

	// A link with the key sets the cookie and drops the key from the address.
	rec := do("GET", "/?key="+key, nil)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Set-Cookie"), "SameSite=Strict") {
		t.Errorf("keyed link: %d %q", rec.Code, rec.Header().Get("Set-Cookie"))
	}
	// Without the key, the page offers only Open Archivist Sync, which opens the keyed page.
	if body := do("GET", "/", nil).Body.String(); !strings.Contains(body, "Open Archivist Sync") || strings.Contains(body, key) {
		t.Error("the keyless page shows more than the launcher, or the key")
	}
	if code := do("POST", "/launch", map[string]string{"Origin": "https://evil.example"}).Code; code != 403 {
		t.Errorf("launch from another site: %d", code)
	}
	if code := do("POST", "/launch", map[string]string{"Origin": "http://127.0.0.1:47811"}).Code; code != 202 {
		t.Errorf("launch: %d", code)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.opened) != 1 || !strings.HasSuffix(w.opened[0], "/?key="+key) {
		t.Errorf("opened %v", w.opened)
	}
}
