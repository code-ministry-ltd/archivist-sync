// Package archivetest is a fake Archivist for tests: device sign-in, Archivist Sync's folders
// and the sync protocol, kept in memory.
package archivetest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Fake is an archive with one user.
type Fake struct {
	*httptest.Server
	mu sync.Mutex
	// Docs holds each root's documents: path → hash.
	Docs map[string]map[string]string
	// Roots is what this computer may sync, with Remove them flags.
	Roots map[string]*Root
	// Uploads lists every path sent, as root/path.
	Uploads []string
	Commits int
	// Approve, when true, approves a sign-in at its first poll.
	Approve  bool
	Token    string
	Revoked  bool
	manifest map[string]map[string]string
	staged   map[string]map[string]string
}

// Root is one folder's state on the archive.
type Root struct {
	WaitingRemovals int
	AllowRemoval    bool
}

// New starts a fake archive.
func New() *Fake {
	f := &Fake{
		Docs: map[string]map[string]string{}, Roots: map[string]*Root{},
		manifest: map[string]map[string]string{}, staged: map[string]map[string]string{},
		Approve: true, Token: "arch_faketoken",
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": code}})
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	if strings.Contains(r.Header.Get("content-type"), "json") {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	switch r.URL.Path {
	case "/api/device/start":
		writeJSON(w, 200, map[string]any{"deviceCode": "dc", "userCode": "KDWF-QRMT",
			"verificationUrl": f.URL + "/connect?code=KDWF-QRMT", "interval": 0, "expiresIn": 600})
		return
	case "/api/device/token":
		if f.Approve {
			writeJSON(w, 200, map[string]string{"status": "approved", "token": f.Token})
		} else {
			writeJSON(w, 200, map[string]string{"status": "declined"})
		}
		return
	}
	if f.Revoked || r.Header.Get("authorization") != "Bearer "+f.Token {
		fail(w, 401, "invalid_token")
		return
	}
	switch {
	case r.URL.Path == "/api/tokens/current" && r.Method == http.MethodDelete:
		f.Revoked = true
		writeJSON(w, 200, map[string]bool{"ok": true})
	case r.URL.Path == "/api/sync/roots" && r.Method == http.MethodGet:
		roots := []map[string]any{}
		for name, s := range f.Roots {
			roots = append(roots, map[string]any{"root": name, "waitingRemovals": s.WaitingRemovals, "allowRemoval": s.AllowRemoval})
		}
		writeJSON(w, 200, map[string]any{"roots": roots})
	case r.URL.Path == "/api/sync/roots" && r.Method == http.MethodPost:
		root, _ := body["root"].(string)
		existing, _ := body["existing"].(bool)
		if _, ok := f.Roots[root]; ok {
			writeJSON(w, 200, map[string]any{"root": root})
			return
		}
		if _, ok := f.Docs[root]; ok && !existing {
			fail(w, 409, "root_exists")
			return
		}
		f.Roots[root] = &Root{}
		writeJSON(w, 201, map[string]any{"root": root})
	case strings.HasPrefix(r.URL.Path, "/api/sync/roots/"):
		root := strings.TrimPrefix(r.URL.Path, "/api/sync/roots/")
		s, ok := f.Roots[root]
		if !ok {
			fail(w, 404, "not_found")
			return
		}
		if r.Method == http.MethodDelete {
			delete(f.Roots, root)
		} else {
			n, _ := body["waitingRemovals"].(float64)
			s.WaitingRemovals = int(n)
			if n == 0 {
				s.AllowRemoval = false
			}
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case r.URL.Path == "/api/sync/manifest":
		root, _ := body["root"].(string)
		if _, ok := f.Roots[root]; !ok {
			fail(w, 403, "wrong_root")
			return
		}
		page, _ := body["page"].(float64)
		if page == 0 {
			f.manifest[root] = map[string]string{}
		}
		upload := []string{}
		unchanged := 0
		entries, _ := body["entries"].([]any)
		for _, e := range entries {
			m := e.(map[string]any)
			p, h := m["path"].(string), m["hash"].(string)
			f.manifest[root][p] = h
			if f.Docs[root][p] == h {
				unchanged++
			} else {
				upload = append(upload, p)
			}
		}
		deletes := 0
		for p := range f.Docs[root] {
			if _, ok := f.manifest[root][p]; !ok {
				deletes++
			}
		}
		writeJSON(w, 200, map[string]any{"upload": upload, "tooLarge": []string{}, "unchanged": unchanged, "deleteCount": deletes})
	case r.URL.Path == "/api/sync/files":
		root, p := r.URL.Query().Get("root"), r.URL.Query().Get("path")
		if _, ok := f.Roots[root]; !ok {
			fail(w, 403, "wrong_root")
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if f.staged[root] == nil {
			f.staged[root] = map[string]string{}
		}
		f.staged[root][p] = f.manifest[root][p]
		_ = raw
		f.Uploads = append(f.Uploads, root+"/"+p)
		writeJSON(w, 200, map[string]any{"path": p})
	case r.URL.Path == "/api/sync/commit":
		root, _ := body["root"].(string)
		if f.Docs[root] == nil {
			f.Docs[root] = map[string]string{}
		}
		for p, h := range f.staged[root] {
			f.Docs[root][p] = h
		}
		for p := range f.Docs[root] {
			if _, ok := f.manifest[root][p]; !ok {
				delete(f.Docs[root], p)
			}
		}
		f.staged[root] = nil
		f.Commits++
		writeJSON(w, 202, map[string]string{"importId": "imp"})
	default:
		fail(w, 404, "not_found")
	}
}

// Lock runs fn while holding the fake's lock, to read or change its state safely.
func (f *Fake) Lock(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}
