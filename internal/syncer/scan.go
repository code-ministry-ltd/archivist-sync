// Package syncer turns a folder on this computer into a manifest and syncs it to the archive
// (spec 0012 §6): one way, read-only, behind the deletion guard.
package syncer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/code-ministry-ltd/archivist-sync/internal/archive"
	"github.com/code-ministry-ltd/archivist-sync/internal/rules"
)

// LeftOut is a file the folder holds but won't send, and why: "format" or "too_large".
type LeftOut struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Scan is a folder as it is now.
type Scan struct {
	Entries []archive.Entry `json:"-"`
	LeftOut []LeftOut       `json:"leftOut"`
	// Files is how many will be sent; LeftOutCount how many won't.
	Files        int `json:"files"`
	LeftOutCount int `json:"leftOutCount"`
}

// Stamp is a file's size and modification time: whether it might have changed.
type Stamp struct {
	Size    int64 `json:"size"`
	ModTime int64 `json:"mtime"`
}

// cached is a file's stamp and the hash it had then.
type cached struct {
	Stamp
	Hash string `json:"hash"`
}

// Cache remembers each file's hash by its stamp, so unchanged files aren't read again.
type Cache struct {
	path  string
	files map[string]cached
}

// OpenCache loads a folder's hash cache from file, or starts an empty one.
func OpenCache(file string) *Cache {
	c := &Cache{path: file, files: map[string]cached{}}
	if raw, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(raw, &c.files)
	}
	return c
}

// Save writes the cache, readable only by this user.
func (c *Cache) Save() error {
	raw, err := json.Marshal(c.files)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// MaxLeftOutListed is how many left-out files a scan names; the rest are only counted.
const MaxLeftOutListed = 200

// ErrMissing means the folder isn't there: an unplugged drive, or moved away.
var ErrMissing = errors.New("the folder is missing")

// walk calls fn for every file the folder might send, with its relative path, and records
// what's left out. Hidden files and folders, clutter and links are skipped silently.
func walk(dir string, fn func(rel string, info fs.FileInfo) error) ([]LeftOut, int, error) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, 0, ErrMissing
	}
	var left []LeftOut
	leftCount := 0
	note := func(rel, reason string) {
		leftCount++
		if len(left) < MaxLeftOutListed {
			left = append(left, LeftOut{Path: rel, Reason: reason})
		}
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable folder inside: skip it rather than fail the sync.
			if d != nil && d.IsDir() && p != dir {
				return fs.SkipDir
			}
			return nil
		}
		if p == dir {
			return nil
		}
		name := d.Name()
		if rules.Hidden(name) || rules.Clutter(name) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !rules.Takes(rel) {
			note(rel, "format")
			return nil
		}
		if !rules.SafePath(rel) {
			note(rel, "name")
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if fi.Size() > rules.MaxFileBytes {
			note(rel, "too_large")
			return nil
		}
		return fn(rel, fi)
	})
	return left, leftCount, err
}

// Stamps lists each file the folder would send with its stamp: a cheap look for changes.
func Stamps(dir string) (map[string]Stamp, error) {
	out := map[string]Stamp{}
	_, _, err := walk(dir, func(rel string, fi fs.FileInfo) error {
		out[rel] = Stamp{Size: fi.Size(), ModTime: fi.ModTime().UnixNano()}
		return nil
	})
	return out, err
}

// ScanFolder lists the folder's files with their hashes, reading only those whose stamp changed.
func ScanFolder(dir string, cache *Cache) (Scan, error) {
	var entries []archive.Entry
	seen := map[string]cached{}
	left, leftCount, err := walk(dir, func(rel string, fi fs.FileInfo) error {
		stamp := Stamp{Size: fi.Size(), ModTime: fi.ModTime().UnixNano()}
		hash := ""
		if c, ok := cache.files[rel]; ok && c.Stamp == stamp {
			hash = c.Hash
		} else {
			h, err := hashFile(filepath.Join(dir, filepath.FromSlash(rel)))
			if err != nil {
				// Being written, or gone since the walk saw it: the next sync tries again.
				return nil
			}
			hash = h
		}
		seen[rel] = cached{Stamp: stamp, Hash: hash}
		entries = append(entries, archive.Entry{Path: rel, Hash: hash, Size: fi.Size()})
		return nil
	})
	if err != nil {
		return Scan{}, err
	}
	cache.files = seen
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return Scan{Entries: entries, LeftOut: left, Files: len(entries), LeftOutCount: leftCount}, nil
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readFile reads a file the scan listed, by its relative path.
func readFile(dir, rel string) ([]byte, error) {
	if strings.Contains(rel, "..") {
		return nil, errors.New("bad path")
	}
	return os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
}

// Preview is what a folder would send, counted without reading any file: for the page, before
// Start syncing.
func Preview(dir string) (Scan, error) {
	files := 0
	left, leftCount, err := walk(dir, func(string, fs.FileInfo) error {
		files++
		return nil
	})
	if err != nil {
		return Scan{}, err
	}
	return Scan{Files: files, LeftOut: left, LeftOutCount: leftCount}, nil
}
