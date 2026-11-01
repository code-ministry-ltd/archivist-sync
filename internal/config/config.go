// Package config keeps Archivist Sync's settings on this computer: the archive it syncs to,
// its token, the page's key and the folders it syncs (spec 0012 §4, §5). The file is readable
// only by this user.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// DefaultArchive is the app the program syncs to unless ARCHIVIST_URL says otherwise.
const DefaultArchive = "https://app.the-archivist.cc"

// Folder is one folder on this computer and the archive folder (root) it syncs to.
type Folder struct {
	Path string `json:"path"`
	Root string `json:"root"`
}

// Settings is everything the program remembers.
type Settings struct {
	Archive string   `json:"archive"`
	Token   string   `json:"token,omitempty"`
	Device  string   `json:"device"`
	Key     string   `json:"key"`
	Port    int      `json:"port,omitempty"`
	Folders []Folder `json:"folders"`
}

// Store reads and writes the settings file, safely from several goroutines.
type Store struct {
	mu   sync.Mutex
	dir  string
	data Settings
}

// Dir is where the program keeps its settings and caches: the user's config folder, under
// "Archivist Sync" on macOS and Windows and "archivist-sync" on Linux. ARCHIVIST_SYNC_HOME
// overrides it, for tests and for running two copies side by side.
func Dir() (string, error) {
	if dir := os.Getenv("ARCHIVIST_SYNC_HOME"); dir != "" {
		return dir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "linux" {
		return filepath.Join(base, "archivist-sync"), nil
	}
	return filepath.Join(base, "Archivist Sync"), nil
}

// Open loads the settings in dir, making them, with a fresh page key, on first use.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	raw, err := os.ReadFile(s.file())
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.data = Settings{}
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &s.data); err != nil {
			return nil, err
		}
	}
	changed := false
	if s.data.Key == "" {
		s.data.Key = randomKey()
		changed = true
	}
	if url := os.Getenv("ARCHIVIST_URL"); url != "" && url != s.data.Archive {
		s.data.Archive = url
		changed = true
	}
	if s.data.Archive == "" {
		s.data.Archive = DefaultArchive
		changed = true
	}
	if s.data.Folders == nil {
		s.data.Folders = []Folder{}
	}
	if changed {
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func randomKey() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Store) file() string { return filepath.Join(s.dir, "settings.json") }

// Dir is the folder holding the settings and caches.
func (s *Store) Dir() string { return s.dir }

// Get returns a copy of the settings.
func (s *Store) Get() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.data
	c.Folders = append([]Folder(nil), s.data.Folders...)
	return c
}

// Update changes the settings and saves them.
func (s *Store) Update(change func(*Settings)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(&s.data)
	return s.save()
}

// save writes the file atomically, readable only by this user.
func (s *Store) save() error {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.file() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.file())
}
