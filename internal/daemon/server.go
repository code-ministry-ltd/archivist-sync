package daemon

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/code-ministry-ltd/archivist-sync/internal/archive"
	"github.com/code-ministry-ltd/archivist-sync/internal/syncer"
	"github.com/code-ministry-ltd/archivist-sync/internal/system"
)

// The page (spec 0012 §4) answers only on this computer, to requests naming its own host, and
// only with its key: a cookie set by a link carrying the key, or a header from the commands.
// Changes must come from the page itself or from the commands, never from another site.

//go:embed page.html
var pageHTML []byte

//go:embed launcher.html
var launcherHTML []byte

// DefaultPort is where the page is served, or the next port up if that's taken.
const DefaultPort = 47811

// KeyHeader carries the key from the commands.
const KeyHeader = "X-Archivist-Sync-Key"

const keyCookie = "archivist_sync_key"

// Server serves the page and the commands' API.
type Server struct {
	d    *Daemon
	port int
	key  string
	// launched limits Open Archivist Sync to one browser tab every few seconds.
	mu       sync.Mutex
	launched time.Time
}

// NewServer serves d on port, with the settings' key.
func NewServer(d *Daemon, port int) *Server {
	return &Server{d: d, port: port, key: d.store.Get().Key}
}

func (s *Server) hosts() map[string]bool {
	p := strconv.Itoa(s.port)
	return map[string]bool{"127.0.0.1:" + p: true, "localhost:" + p: true}
}

func (s *Server) origins() map[string]bool {
	p := strconv.Itoa(s.port)
	return map[string]bool{"http://127.0.0.1:" + p: true, "http://localhost:" + p: true}
}

// KeyedURL opens the page with its key.
func (s *Server) KeyedURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(s.port) + "/?key=" + s.key
}

func (s *Server) keyOK(r *http.Request) bool {
	given := r.Header.Get(KeyHeader)
	if given == "" {
		if c, err := r.Cookie(keyCookie); err == nil {
			given = c.Value
		}
	}
	return given != "" && subtle.ConstantTimeCompare([]byte(given), []byte(s.key)) == 1
}

// sameSite reports whether a change comes from the page itself, or from a command (which sends
// no Origin but must send the key header).
func (s *Server) sameSite(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get(KeyHeader) != "" && r.Header.Get("Sec-Fetch-Site") == ""
	}
	return s.origins()[origin]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// Handler is the page and its API, behind the host, origin and key checks.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("POST /launch", s.launch)
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]string{"app": "archivist-sync", "version": s.d.version})
	})
	api := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if !s.keyOK(r) {
				fail(w, 401, "Open Archivist Sync from its shortcut, or run archivist-sync open.")
				return
			}
			if r.Method != http.MethodGet && !s.sameSite(r) {
				fail(w, 403, "Changes must come from the Archivist Sync page.")
				return
			}
			h(w, r)
		})
	}
	api("GET /api/status", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, s.d.Status()) })
	api("GET /api/suggestions", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"folders": s.d.hooks.Suggestions()})
	})
	api("GET /api/preview", s.preview)
	api("POST /api/connect", s.connect)
	api("POST /api/disconnect", func(w http.ResponseWriter, r *http.Request) {
		s.d.Disconnect(r.Context())
		writeJSON(w, 200, s.d.Status())
	})
	api("POST /api/folders", s.addFolder)
	api("DELETE /api/folders", func(w http.ResponseWriter, r *http.Request) {
		root, err := s.d.Remove(r.Context(), r.URL.Query().Get("folder"))
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"root": root})
	})
	api("POST /api/sync", func(w http.ResponseWriter, r *http.Request) {
		if err := s.d.SyncNow(r.URL.Query().Get("folder")); err != nil {
			fail(w, 400, err.Error())
			return
		}
		writeJSON(w, 202, map[string]bool{"ok": true})
	})
	api("POST /api/allow-removal", func(w http.ResponseWriter, r *http.Request) {
		if err := s.d.AllowRemoval(r.URL.Query().Get("folder")); err != nil {
			fail(w, 400, err.Error())
			return
		}
		writeJSON(w, 202, map[string]bool{"ok": true})
	})
	api("POST /api/pick", func(w http.ResponseWriter, _ *http.Request) {
		path, err := s.d.hooks.PickFolder("Choose a folder to sync to your archive")
		if errors.Is(err, system.ErrNoPicker) {
			writeJSON(w, 200, map[string]any{"path": "", "noPicker": true})
			return
		}
		writeJSON(w, 200, map[string]any{"path": path})
	})
	api("POST /api/update", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"note": s.d.CheckUpdate(r.Context(), false)})
	})
	api("POST /api/uninstall", func(w http.ResponseWriter, r *http.Request) {
		if err := s.d.Uninstall(r.Context()); err != nil {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// DNS rebinding: a page on another name pointed at this computer is refused.
		if !s.hosts()[r.Host] {
			http.Error(w, "Archivist Sync answers only at 127.0.0.1.", http.StatusMisdirectedRequest)
			return
		}
		w.Header().Set("x-content-type-options", "nosniff")
		w.Header().Set("x-frame-options", "DENY")
		w.Header().Set("referrer-policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	if key := r.URL.Query().Get("key"); key != "" {
		if subtle.ConstantTimeCompare([]byte(key), []byte(s.key)) == 1 {
			http.SetCookie(w, &http.Cookie{Name: keyCookie, Value: s.key, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		}
		// Drop the key from the address bar and history.
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	w.Header().Set("content-security-policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'")
	if s.keyOK(r) {
		_, _ = w.Write(pageHTML)
		return
	}
	_, _ = w.Write(launcherHTML)
}

// launch is Open Archivist Sync from the keyless page: the program opens its keyed page itself,
// for its own user, so the key is never shown to anyone else (spec 0012 §4).
func (s *Server) launch(w http.ResponseWriter, r *http.Request) {
	if !s.origins()[r.Header.Get("Origin")] {
		fail(w, 403, "Changes must come from the Archivist Sync page.")
		return
	}
	s.mu.Lock()
	if time.Since(s.launched) < 3*time.Second {
		s.mu.Unlock()
		writeJSON(w, 202, map[string]bool{"ok": true})
		return
	}
	s.launched = time.Now()
	s.mu.Unlock()
	_ = s.d.hooks.OpenBrowser(s.KeyedURL())
	writeJSON(w, 202, map[string]bool{"ok": true})
}

func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	scan, err := syncer.Preview(path)
	if err != nil {
		fail(w, 400, "That isn't a folder on this computer.")
		return
	}
	writeJSON(w, 200, map[string]any{"files": scan.Files, "leftOutCount": scan.LeftOutCount, "leftOut": scan.LeftOut, "root": suggestRoot(path)})
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	c, err := s.d.Connect(ctx)
	if err != nil {
		fail(w, 502, "Couldn't reach your archive. Check the internet connection and try again.")
		return
	}
	if r.URL.Query().Get("open") == "1" {
		_ = s.d.hooks.OpenBrowser(c.URL)
	}
	writeJSON(w, 200, c)
}

func (s *Server) addFolder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path     string `json:"path"`
		Root     string `json:"root"`
		Existing bool   `json:"existing"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
		fail(w, 400, "Send the folder's path.")
		return
	}
	f, err := s.d.Add(r.Context(), body.Path, body.Root, body.Existing)
	if err != nil {
		var ae *archive.Error
		switch {
		case errors.As(err, &ae) && ae.Code == "root_exists":
			fail(w, 409, ae.Message)
		case errors.As(err, &ae):
			fail(w, ae.Status, ae.Error())
		default:
			fail(w, 400, err.Error())
		}
		return
	}
	writeJSON(w, 201, f)
}

// Listen binds the page's port on this computer only: DefaultPort or the next free one up. A
// port held by another Archivist Sync is reported, so a second copy doesn't start.
func Listen(preferred int) (net.Listener, int, error) {
	if preferred == 0 {
		preferred = DefaultPort
	}
	var last error
	for port := preferred; port < preferred+20; port++ {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil {
			return l, port, nil
		}
		last = err
		if Running(port) {
			return nil, port, ErrRunning
		}
	}
	return nil, 0, last
}

// ErrRunning means Archivist Sync is already running on this computer.
var ErrRunning = errors.New("Archivist Sync is already running")

// Running reports whether Archivist Sync answers on port.
func Running(port int) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	res, err := c.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/api/ping")
	if err != nil {
		return false
	}
	defer res.Body.Close()
	var out struct {
		App string `json:"app"`
	}
	return json.NewDecoder(res.Body).Decode(&out) == nil && out.App == "archivist-sync"
}
